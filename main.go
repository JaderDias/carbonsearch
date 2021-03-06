package main

// handle virt. namespace metric requests from carbon zipper

import (
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kanatohodets/carbonsearch/consumer"
	"github.com/kanatohodets/carbonsearch/consumer/httpapi"
	"github.com/kanatohodets/carbonsearch/consumer/kafka"
	"github.com/kanatohodets/carbonsearch/database"
	"github.com/kanatohodets/carbonsearch/util"

	pb "github.com/dgryski/carbonzipper/carbonzipperpb"
	"github.com/dgryski/carbonzipper/mlog"
	"github.com/dgryski/carbonzipper/mstats"

	"github.com/NYTimes/gziphandler"
	"github.com/gogo/protobuf/proto"
	"github.com/peterbourgon/g2g"
)

// BuildVersion is provided to be overridden at build time. Eg. go build -ldflags -X 'main.BuildVersion=...'
var BuildVersion = "(development build)"

// Config hold configuration variables
var Config = struct {
	Prefix      string `yaml:"prefix"`
	Buckets     int    `yaml:"buckets"`
	Port        int    `yaml:"port"`
	IntervalSec int    `yaml:"interval_sec"`
	QueryLimit  int    `yaml:"query_limit"`
	ResultLimit int    `yaml:"result_limit"`

	IndexRotationRate string            `yaml:"index_rotation_rate"`
	GraphiteHost      string            `yaml:"graphite_host"`
	Consumers         map[string]string `yaml:"consumers"`

	FullIndexService string            `yaml:"full_index_service"`
	TextIndexService string            `yaml:"text_index_service"`
	SplitIndexes     map[string]string `yaml:"split_indexes"`
}{
	Port: 8070,

	Buckets: 10,

	QueryLimit:  100,
	ResultLimit: 20000,

	IndexRotationRate: "60s",
}

var db *database.Database

var stats *util.Stats

var logger mlog.Level

var virtPrefix string

var timeBuckets []int64

type bucketEntry int

func (b bucketEntry) String() string {
	return strconv.Itoa(int(atomic.LoadInt64(&timeBuckets[b])))
}

func renderTimeBuckets() interface{} {
	return timeBuckets
}

func bucketRequestTimes(req *http.Request, t time.Duration) {

	ms := t.Nanoseconds() / int64(time.Millisecond)

	bucket := int(ms / 100)

	if bucket < Config.Buckets {
		atomic.AddInt64(&timeBuckets[bucket], 1)
	} else {
		// Too big? Increment overflow bucket and log
		atomic.AddInt64(&timeBuckets[Config.Buckets], 1)
		logger.Logf("Slow Request: %s: %s", t.String(), req.URL.String())
	}
}

// virt.v1.*.serv*
// -> serv*
func handleAutocomplete(rawQuery, trimmedQuery string) (pb.GlobResponse, error) {
	tags := strings.Split(trimmedQuery, ".")
	completionTag := tags[len(tags)-1]
	completions := db.Autocomplete(completionTag)
	var result pb.GlobResponse

	result.Name = &rawQuery
	result.Matches = make([]*pb.GlobMatch, 0, len(completions))
	base := fmt.Sprintf("%s%s", virtPrefix, strings.Join(tags[:len(tags)-1], "."))
	base = strings.TrimSuffix(base, ".")
	for _, completion := range completions {
		full := fmt.Sprintf("%s.%s", base, completion)
		result.Matches = append(result.Matches, &pb.GlobMatch{Path: proto.String(full), IsLeaf: proto.Bool(false)})
	}

	return result, nil
}

func handleQuery(rawQuery string, query map[string][]string) (pb.GlobResponse, error) {
	metrics, err := db.Query(query)
	var result pb.GlobResponse
	if err != nil {
		return result, err
	}

	result.Name = &rawQuery
	result.Matches = make([]*pb.GlobMatch, 0, len(metrics))
	for _, metric := range metrics {
		result.Matches = append(result.Matches, &pb.GlobMatch{Path: proto.String(metric), IsLeaf: proto.Bool(true)})
	}

	return result, nil
}

func findHandler(w http.ResponseWriter, req *http.Request) {
	uri, _ := url.ParseRequestURI(req.URL.RequestURI())
	uriQuery := uri.Query()

	stats.QueriesHandled.Add(1)
	queries := uriQuery["query"]
	if len(queries) != 1 {
		err := fmt.Errorf("req validation: there must be exactly one 'query' url param")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	formats := uriQuery["format"]
	if len(formats) != 1 {
		err := fmt.Errorf("req validation: there must be exactly one 'format' url param")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	format := formats[0]
	if format != "protobuf" && format != "json" {
		err := fmt.Errorf("main: %q is not a recognized format: known formats are 'protobuf' and 'json'", format)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rawQuery := queries[0]
	if !strings.HasPrefix(rawQuery, virtPrefix) {
		err := fmt.Errorf("main: the query is not a valid virtual metric (must start with %q): %s", virtPrefix, rawQuery)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	trimmedQuery := strings.TrimPrefix(rawQuery, virtPrefix)

	var result pb.GlobResponse
	// query = serv*
	// query = *
	// query = servers-*
	// query = servers-stat*
	// query = servers-status:*
	// query = servers-status:live.*
	if strings.HasSuffix(trimmedQuery, "*") {
		var err error
		result, err = handleAutocomplete(rawQuery, trimmedQuery)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		queryTags, err := db.ParseQuery(trimmedQuery)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		result, err = handleQuery(rawQuery, queryTags)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if format == "protobuf" {
		w.Header().Set("Content-Type", "application/x-protobuf")
		b, _ := result.Marshal()
		w.Write(b)
	} else if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		err := enc.Encode(result)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func main() {
	configPath := flag.String("config", "carbonsearch.yaml", "Path to the `config file`.")
	blockingProfile := flag.String("blockProfile", "", "Path to `block profile output file`. Block profiler disabled if empty.")
	cpuProfile := flag.String("cpuProfile", "", "Path to `cpu profile output file`. CPU profiler disabled if empty.")
	interval := flag.Duration("i", 0, "interval to report internal statistics to graphite")
	flag.Parse()

	if *configPath == "" {
		printUsageErrorAndExit("Can't run without a config file")
	}

	if *blockingProfile != "" {
		f, err := os.Create(*blockingProfile)
		if err != nil {
			logger.Fatalln(err.Error())
		}
		runtime.SetBlockProfileRate(1)
		defer f.Close()
		defer pprof.Lookup("block").WriteTo(f, 1)
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			logger.Fatalln(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	err := util.ReadConfig(*configPath, &Config)
	if err != nil {
		printErrorAndExit(1, "could not read config: %s", err)
	}

	if *interval == 0 {
		*interval = time.Duration(Config.IntervalSec) * time.Second
	}

	if len(Config.Prefix) == 0 {
		printErrorAndExit(1, "carbonsearch.yaml must define the query prefix (usually something like virt.v1.*.)")
	}

	if !strings.HasSuffix(Config.Prefix, ".") {
		printErrorAndExit(1, "config error: 'prefix' must terminate with '.'. The current value is: %q", Config.Prefix)
	}

	virtPrefix = Config.Prefix

	strikes := 0
	if len(Config.SplitIndexes) == 0 {
		strikes++
		logger.Logln("warning: config doesn't have any split indexes defined")
	}

	if len(Config.FullIndexService) == 0 {
		strikes++
		logger.Logln("warning: full index service is empty. disabling direct tag<->index associations.")
	}

	if len(Config.TextIndexService) == 0 {
		strikes++
		logger.Logln("warning: text index service is empty. disabling text index.")
	}

	if strikes == 3 {
		printErrorAndExit(1, "config doesn't have any valid indexes. Please double check the config file (%q).", *configPath)
	}

	if len(Config.Consumers) == 0 {
		printErrorAndExit(1, "config doesn't have any consumers. carbonsearch won't have anything to search on. Take a peek in %q, see if it looks like it should", *configPath)
	}

	stats = util.InitStats()

	// nothing in the config? check the environment
	if Config.GraphiteHost == "" {
		if host := os.Getenv("GRAPHITEHOST") + ":" + os.Getenv("GRAPHITEPORT"); host != ":" {
			Config.GraphiteHost = host
		}
	}
	if Config.GraphiteHost != "" {
		logger.Logln("Using graphite host", Config.GraphiteHost)
		graphite := g2g.NewGraphite(Config.GraphiteHost, *interval, 10*time.Second)

		hostname, _ := os.Hostname()
		hostname = strings.Replace(hostname, ".", "_", -1)

		graphite.Register(fmt.Sprintf("carbon.search.%s.custom_messages", hostname), stats.CustomMessages)
		graphite.Register(fmt.Sprintf("carbon.search.%s.metric_indexed", hostname), stats.MetricsIndexed)
		graphite.Register(fmt.Sprintf("carbon.search.%s.metric_messages", hostname), stats.MetricMessages)
		graphite.Register(fmt.Sprintf("carbon.search.%s.requests", hostname), stats.QueriesHandled)
		graphite.Register(fmt.Sprintf("carbon.search.%s.tag_indexed", hostname), stats.TagsIndexed)
		graphite.Register(fmt.Sprintf("carbon.search.%s.tag_messages", hostname), stats.TagMessages)
		graphite.Register(fmt.Sprintf("carbon.search.%s.uptime", hostname), stats.Uptime)
		graphite.Register(fmt.Sprintf("carbon.search.%s.full_tags", hostname), stats.FullIndexTags)
		graphite.Register(fmt.Sprintf("carbon.search.%s.full_metrics", hostname), stats.FullIndexMetrics)

		// Split index metrics
		for idx := range Config.SplitIndexes {
			graphite.Register(fmt.Sprintf("carbon.search.%s.split_index.%s.generation", hostname, idx), expvar.Func(func() interface{} { return stats.SplitIndexes.Get(idx + "-generation") }))
			graphite.Register(fmt.Sprintf("carbon.search.%s.split_index.%s.generation_time", hostname, idx), expvar.Func(func() interface{} { return stats.SplitIndexes.Get(idx + "-generation-time") }))
			graphite.Register(fmt.Sprintf("carbon.search.%s.split_index.%s.join", hostname, idx), expvar.Func(func() interface{} { return stats.SplitIndexes.Get(idx + "-join") }))
			graphite.Register(fmt.Sprintf("carbon.search.%s.split_index.%s.metrics", hostname, idx), expvar.Func(func() interface{} { return stats.SplitIndexes.Get(idx + "-metrics") }))
			graphite.Register(fmt.Sprintf("carbon.search.%s.split_index.%s.tags", hostname, idx), expvar.Func(func() interface{} { return stats.SplitIndexes.Get(idx + "-tags") }))
		}

		// Text index metrics
		graphite.Register(fmt.Sprintf("carbon.search.%s.text_index.generation", hostname), expvar.Func(func() interface{} { return stats.TextIndex.Get("generation") }))
		graphite.Register(fmt.Sprintf("carbon.search.%s.text_index.generation_time", hostname), expvar.Func(func() interface{} { return stats.TextIndex.Get("generation-time") }))
		graphite.Register(fmt.Sprintf("carbon.search.%s.text_index.metrics_readable", hostname), expvar.Func(func() interface{} { return stats.TextIndex.Get("metrics-readable") }))

		// full index metrics
		graphite.Register(fmt.Sprintf("carbon.search.%s.full_index.generation", hostname), expvar.Func(func() interface{} { return stats.FullIndex.Get("generation") }))
		graphite.Register(fmt.Sprintf("carbon.search.%s.full_index.generation_time", hostname), expvar.Func(func() interface{} { return stats.FullIndex.Get("generation-time") }))
		graphite.Register(fmt.Sprintf("carbon.search.%s.full_index.tags_readable", hostname), expvar.Func(func() interface{} { return stats.FullIndex.Get("tags-readable") }))

		// +1 to track every over the number of buckets we track
		timeBuckets = make([]int64, Config.Buckets+1)

		for i := 0; i < Config.Buckets; i++ {
			graphite.Register(fmt.Sprintf("carbon.search.%s.requests_in_%dms_to_%dms", hostname, i*100, (i+1)*100), bucketEntry(i))
		}
		graphite.Register(fmt.Sprintf("carbon.search.%s.requests_in_%dms_to_infinity", hostname, Config.Buckets*100), bucketEntry(Config.Buckets))

		go mstats.Start(*interval)

		graphite.Register(fmt.Sprintf("carbon.search.%s.alloc", hostname), &mstats.Alloc)
		graphite.Register(fmt.Sprintf("carbon.search.%s.total_alloc", hostname), &mstats.TotalAlloc)
		graphite.Register(fmt.Sprintf("carbon.search.%s.num_gc", hostname), &mstats.NumGC)
		graphite.Register(fmt.Sprintf("carbon.search.%s.pause_ns", hostname), &mstats.PauseNS)

	}

	wg := &sync.WaitGroup{}
	db = database.New(
		Config.QueryLimit,
		Config.ResultLimit,
		Config.FullIndexService,
		Config.TextIndexService,
		Config.SplitIndexes,
		stats,
	)
	quit := make(chan bool)

	constructors := map[string]func(string) (consumer.Consumer, error){
		"kafka": func(confPath string) (consumer.Consumer, error) {
			c, err := kafka.New(confPath)
			return c, err
		},
		"httpapi": func(confPath string) (consumer.Consumer, error) {
			c, err := httpapi.New(confPath)
			return c, err
		},
	}

	consumers := []consumer.Consumer{}
	for consumerName, consumerConfigPath := range Config.Consumers {
		constructor, ok := constructors[consumerName]
		if !ok {
			printErrorAndExit(1, "carbonsearch doesn't know how to create consumer %q. talk to the authors, or remove %q from the list of consumers in %q", consumerName, consumerName, *configPath)
		}
		consumer, err := constructor(consumerConfigPath)
		if err != nil {
			printErrorAndExit(1, "could not create new %s consumer: %s", consumerName, err)
		}

		err = consumer.Start(wg, db)
		if err != nil {
			printErrorAndExit(1, "could not start %s consumer: %s", consumerName, err)
		}

		consumers = append(consumers, consumer)
	}

	rotationRate, err := time.ParseDuration(Config.IndexRotationRate)
	if err != nil {
		printErrorAndExit(1, "config index_rotation_rate %q cannot be parsed as a duration. Please check https://golang.org/pkg/time/#ParseDuration for valid expressions", Config.IndexRotationRate)
	}

	go func() {
		for {
			time.Sleep(rotationRate)
			db.MaterializeIndexes()
		}
	}()

	go func() {
		http.HandleFunc("/metrics/find/", findHandler)

		http.HandleFunc("/admin/toc/", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			err = enc.Encode(db.TableOfContents())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})

		http.HandleFunc("/admin/metric_list/", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			err = enc.Encode(db.MetricList())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})

		portStr := fmt.Sprintf(":%d", Config.Port)
		logger.Logln("Starting carbonsearch", BuildVersion)
		logger.Logf("listening on %s\n", portStr)
		err := http.ListenAndServe(portStr, gziphandler.GzipHandler(http.DefaultServeMux))
		if err != nil {
			logger.Fatalf("failure to start carbonsearch API: %v", err)
		}
	}()

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Kill, os.Interrupt)
		<-signals
		logger.Logln("Shutting down...")
		for _, consumer := range consumers {
			err := consumer.Stop()
			if err != nil {
				logger.Logf("Failed to close consumer %s: %s", consumer.Name(), err)
			}
		}

		quit <- true
	}()

	wg.Wait()
}

func printErrorAndExit(code int, format string, values ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", fmt.Sprintf(format, values...))
	fmt.Fprintln(os.Stderr)
	os.Exit(code)
}

func printUsageErrorAndExit(format string, values ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", fmt.Sprintf(format, values...))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Available command line options:")
	flag.PrintDefaults()
	os.Exit(64)
}
