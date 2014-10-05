package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"code.google.com/p/gogoprotobuf/proto"

	pb "github.com/dgryski/carbonzipper/carbonzipperpb"
	"github.com/dgryski/httputil"
	pickle "github.com/kisielk/og-rek"
	"github.com/peterbourgon/g2g"
)

// configuration values
var Config = struct {
	Backends []string
	MaxProcs int
	Port     int
	Buckets  int

	TimeoutMs               int
	TimeoutMsAfterFirstSeen int

	GraphiteHost string

	mu          sync.RWMutex
	metricPaths map[string][]string

	MaxIdleConnsPerHost int
}{
	MaxProcs: 1,
	Port:     8080,
	Buckets:  10,

	TimeoutMs:               2000,
	TimeoutMsAfterFirstSeen: 500,

	MaxIdleConnsPerHost: 100,

	metricPaths: make(map[string][]string),
}

// grouped expvars for /debug/vars and graphite
var Metrics = struct {
	FindRequests *expvar.Int
	FindErrors   *expvar.Int

	RenderRequests *expvar.Int
	RenderErrors   *expvar.Int

	Timeouts *expvar.Int
}{
	FindRequests: expvar.NewInt("find_requests"),
	FindErrors:   expvar.NewInt("find_errors"),

	RenderRequests: expvar.NewInt("render_requests"),
	RenderErrors:   expvar.NewInt("render_errors"),

	Timeouts: expvar.NewInt("timeouts"),
}

var BuildVersion = "(development version)"

var Limiter serverLimiter

var logger multilog

type serverResponse struct {
	server   string
	response []byte
}

var storageClient = &http.Client{}

func singleGet(uri, server string, ch chan<- serverResponse) {

	u, err := url.Parse(server + uri)
	if err != nil {
		logger.Logln("error parsing uri: ", server+uri, ":", err)
		ch <- serverResponse{server, nil}
		return
	}
	req := http.Request{
		URL:    u,
		Header: make(http.Header),
	}

	Limiter.enter(server)
	defer Limiter.leave(server)
	resp, err := storageClient.Do(&req)
	if err != nil {
		logger.Logln("singleGet: error querying ", server, "/", uri, ":", err)
		ch <- serverResponse{server, nil}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// carbonsserver replies with Not Found if we request a
		// metric that it doesn't have -- makes sense
		ch <- serverResponse{server, nil}
		return
	}

	if resp.StatusCode != 200 {
		logger.Logln("bad response code ", server, "/", uri, ":", resp.StatusCode)
		ch <- serverResponse{server, nil}
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Logln("error reading body: ", server, "/", uri, ":", err)
		ch <- serverResponse{server, nil}
		return
	}

	ch <- serverResponse{server, body}
}

func multiGet(servers []string, uri string) []serverResponse {

	logger.Debugln("querying servers=", servers, "uri=", uri)

	// buffered channel so the goroutines don't block on send
	ch := make(chan serverResponse, len(servers))

	for _, server := range servers {
		go singleGet(uri, server, ch)
	}

	var response []serverResponse

	isFirstResponse := true
	timeout := time.After(time.Duration(Config.TimeoutMs) * time.Millisecond)

GATHER:
	for i := 0; i < len(servers); i++ {
		select {
		case r := <-ch:
			if r.response != nil {

				response = append(response, r)

				if isFirstResponse {
					timeout = time.After(time.Duration(Config.TimeoutMsAfterFirstSeen) * time.Millisecond)
				}
				isFirstResponse = false
			}

		case <-timeout:
			servs := make([]string, 0)
			for _, r := range response {
				servs = append(servs, r.server)
			}
			logger.Logln("Timeout waiting for more responses.  uri=", uri, ", servers=", servers, ", answers_from_servers=", servs)
			Metrics.Timeouts.Add(1)
			break GATHER
		}
	}

	return response
}

func findHandlerPB(w http.ResponseWriter, req *http.Request, responses []serverResponse) ([]*pb.GlobMatch, map[string][]string) {

	// metric -> [server1, ... ]
	paths := make(map[string][]string)

	var metrics []*pb.GlobMatch
	for _, r := range responses {
		var metric pb.GlobResponse
		err := proto.Unmarshal(r.response, &metric)
		if err != nil {
			logger.Logf("error decoding protobuf response from server:%s: req:%s: err=%s", r.server, req.URL.RequestURI(), err)
			logger.Traceln("\n" + hex.Dump(r.response))
			Metrics.FindErrors.Add(1)
			continue
		}

		for _, match := range metric.Matches {
			p, ok := paths[*match.Path]
			if !ok {
				// we haven't seen this name yet
				// add the metric to the list of metrics to return
				metrics = append(metrics, match)
			}
			// add the server to the list of servers that know about this metric
			p = append(p, r.server)
			paths[*match.Path] = p
		}
	}

	return metrics, paths
}

func findHandler(w http.ResponseWriter, req *http.Request) {

	logger.Debugln("request: ", req.URL.RequestURI())

	Metrics.FindRequests.Add(1)

	rewrite, _ := url.ParseRequestURI(req.URL.RequestURI())
	v := rewrite.Query()
	format := req.FormValue("format")
	v.Set("format", "protobuf")
	rewrite.RawQuery = v.Encode()

	responses := multiGet(Config.Backends, rewrite.RequestURI())

	if responses == nil || len(responses) == 0 {
		logger.Logln("find: error querying backends for: ", rewrite.RequestURI())
		http.Error(w, "find: error querying backends", http.StatusInternalServerError)
		return
	}

	metrics, paths := findHandlerPB(w, req, responses)

	// update our cache of which servers have which metrics
	Config.mu.Lock()
	for k, v := range paths {
		Config.metricPaths[k] = v
	}
	Config.mu.Unlock()

	switch format {
	case "protobuf":
		w.Header().Set("Content-Type", "application/protobuf")
		var result pb.GlobResponse
		query := req.FormValue("query")
		result.Name = &query
		result.Matches = metrics
		b, _ := proto.Marshal(&result)
		w.Write(b)
	case "json":
		w.Header().Set("Content-Type", "application/json")
		jEnc := json.NewEncoder(w)
		jEnc.Encode(metrics)
	case "", "pickle":
		w.Header().Set("Content-Type", "application/pickle")

		var result []map[string]interface{}

		for _, metric := range metrics {
			mm := map[string]interface{}{
				"metric_path": *metric.Path,
				"isLeaf":      *metric.IsLeaf,
			}
			result = append(result, mm)
		}

		pEnc := pickle.NewEncoder(w)
		pEnc.Encode(result)
	}
}

func renderHandler(w http.ResponseWriter, req *http.Request) {

	logger.Debugln("request: ", req.URL.RequestURI())

	Metrics.RenderRequests.Add(1)

	req.ParseForm()
	target := req.FormValue("target")

	if target == "" {
		http.Error(w, "empty target", http.StatusBadRequest)
		return
	}

	var serverList []string
	var ok bool

	Config.mu.RLock()
	// lookup the server list for this metric, or use all the servers if it's unknown
	if serverList, ok = Config.metricPaths[target]; !ok || serverList == nil || len(serverList) == 0 {
		serverList = Config.Backends
	}
	Config.mu.RUnlock()

	format := req.FormValue("format")
	rewrite, _ := url.ParseRequestURI(req.URL.RequestURI())
	v := rewrite.Query()
	v.Set("format", "protobuf")
	rewrite.RawQuery = v.Encode()

	responses := multiGet(serverList, rewrite.RequestURI())

	if responses == nil || len(responses) == 0 {
		logger.Logln("render: error querying backends for:", req.URL.RequestURI(), "backends:", serverList)
		http.Error(w, "render: error querying backends", http.StatusInternalServerError)
		Metrics.RenderErrors.Add(1)
		return
	}

	handleRenderPB(w, req, format, responses)
}

func createRenderResponse(metric pb.FetchResponse, missing interface{}) map[string]interface{} {
	var pvalues []interface{}
	for i, v := range metric.Values {
		if metric.IsAbsent[i] {
			pvalues = append(pvalues, missing)
		} else {
			pvalues = append(pvalues, v)
		}
	}

	// create the response
	presponse := map[string]interface{}{
		"start":  metric.StartTime,
		"step":   metric.StepTime,
		"end":    metric.StopTime,
		"name":   metric.Name,
		"values": pvalues,
	}

	return presponse
}

func returnRender(w http.ResponseWriter, format string, metric pb.FetchResponse) {

	switch format {
	case "protobuf":
		w.Header().Set("Content-Type", "application/protobuf")
		b, _ := proto.Marshal(&metric)
		w.Write(b)

	case "json":
		presponse := createRenderResponse(metric, nil)
		w.Header().Set("Content-Type", "application/json")
		e := json.NewEncoder(w)
		e.Encode(presponse)

	case "", "pickle":
		presponse := createRenderResponse(metric, pickle.None{})
		w.Header().Set("Content-Type", "application/pickle")
		e := pickle.NewEncoder(w)
		e.Encode([]interface{}{presponse})
	}

}

func handleRenderPB(w http.ResponseWriter, req *http.Request, format string, responses []serverResponse) {

	var decoded []pb.FetchResponse
	for _, r := range responses {
		var d pb.FetchResponse
		err := proto.Unmarshal(r.response, &d)
		if err != nil {
			logger.Logf("error decoding protobuf response from server:%s: req:%s: err=%s", r.server, req.URL.RequestURI(), err)
			logger.Traceln("\n" + hex.Dump(r.response))
			Metrics.RenderErrors.Add(1)
			continue
		}
		decoded = append(decoded, d)
	}

	logger.Traceln("request: %s: %v", req.URL.RequestURI(), decoded)

	if len(decoded) == 0 {
		err := fmt.Sprintf("no decoded responses to merge for req: %s", req.URL.RequestURI())
		logger.Logln(err)
		http.Error(w, err, http.StatusInternalServerError)
		Metrics.RenderErrors.Add(1)
		return
	}

	if len(decoded) == 1 {
		logger.Debugf("only one decoded responses to merge for req: %s", req.URL.RequestURI())
		returnRender(w, format, decoded[0])
		return
	}

	// Use the metric with the highest resolution as our base
	var highest int
	for i, d := range decoded {
		if d.GetStepTime() < decoded[highest].GetStepTime() {
			highest = i
		}
	}
	decoded[0], decoded[highest] = decoded[highest], decoded[0]

	metric := decoded[0]

	mergeValues(req, &metric, decoded)

	returnRender(w, format, metric)
}

func mergeValues(req *http.Request, metric *pb.FetchResponse, decoded []pb.FetchResponse) {

	var responseLengthMismatch bool
	for i := range metric.Values {
		if !metric.IsAbsent[i] || responseLengthMismatch {
			continue
		}

		// found a missing value, find a replacement
		for other := 1; other < len(decoded); other++ {

			m := decoded[other]

			if len(m.Values) != len(metric.Values) {
				logger.Logf("request: %s: unable to merge ovalues: len(values)=%d but len(ovalues)=%d", req.URL.RequestURI(), len(metric.Values), len(m.Values))
				// TODO(dgryski): we should remove
				// decoded[other] from the list of responses to
				// consider but this assumes that decoded[0] is
				// the 'highest resolution' response and thus
				// the one we want to keep, instead of the one
				// we want to discard

				Metrics.RenderErrors.Add(1)
				responseLengthMismatch = true
				break
			}

			// found one
			if !m.IsAbsent[i] {
				metric.IsAbsent[i] = false
				metric.Values[i] = m.Values[i]
			}
		}
	}
}

func lbCheckHandler(w http.ResponseWriter, req *http.Request) {

	logger.Traceln("loadbalancer: ", req.URL.RequestURI())

	fmt.Fprintf(w, "Ok\n")
}

func stripCommentHeader(cfg []byte) []byte {

	// strip out the comment header block that begins with '#' characters
	// as soon as we see a line that starts with something _other_ than '#', we're done

	idx := 0
	for cfg[0] == '#' {
		idx = bytes.Index(cfg, []byte("\n"))
		if idx == -1 || idx+1 == len(cfg) {
			return nil
		}
		cfg = cfg[idx+1:]
	}

	return cfg
}

func main() {

	configFile := flag.String("c", "", "config file (json)")
	port := flag.Int("p", 0, "port to listen on")
	maxprocs := flag.Int("maxprocs", 0, "GOMAXPROCS")
	debugLevel := flag.Int("d", 0, "enable debug logging")
	logStdout := flag.Bool("stdout", false, "write logging output also to stdout")
	logSyslog := flag.Bool("syslog", true, "write logging output also to syslog")
	concurrencyLimit := flag.Int("limit", 0, "concurrency limit per server (0 to disable)")

	flag.Parse()

	expvar.NewString("BuildVersion").Set(BuildVersion)
	log.Println("starting carbonapi", BuildVersion)

	if *configFile == "" {
		log.Fatal("missing config file")
	}

	cfgjs, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatal("unable to load config file:", err)
	}

	cfgjs = stripCommentHeader(cfgjs)

	if cfgjs == nil {
		log.Fatal("error removing header comment from ", *configFile)
	}

	err = json.Unmarshal(cfgjs, &Config)
	if err != nil {
		log.Fatal("error parsing config file: ", err)
	}

	if len(Config.Backends) == 0 {
		log.Fatal("no Backends loaded -- exiting")
	}

	// command line overrides config file

	if *port != 0 {
		Config.Port = *port
	}

	if *maxprocs != 0 {
		Config.MaxProcs = *maxprocs
	}

	// set up our logging
	logger.level = logLevel(*debugLevel)
	if *logSyslog {
		slog, err := syslog.New(syslog.LOG_DAEMON, "carbonzipper")
		if err != nil {
			log.Fatal("can't obtain a syslog connection", err)
		}
		logger.loggers = append(logger.loggers, &sysLogger{w: slog})
	}

	if *logStdout {
		logger.loggers = append(logger.loggers, &stdoutLogger{log.New(os.Stdout, "", log.LstdFlags)})
	}

	logger.Logln("setting GOMAXPROCS=", Config.MaxProcs)
	runtime.GOMAXPROCS(Config.MaxProcs)

	if *concurrencyLimit != 0 {
		Limiter = newServerLimiter(Config.Backends, *concurrencyLimit)
	}

	// +1 to track every over the number of buckets we track
	timeBuckets = make([]int64, Config.Buckets+1)

	httputil.PublishTrackedConnections("httptrack")
	expvar.Publish("requestBuckets", expvar.Func(renderTimeBuckets))

	// export config via expvars
	expvar.Publish("Config", expvar.Func(func() interface{} { return Config }))

	http.HandleFunc("/metrics/find/", httputil.TrackConnections(httputil.TimeHandler(findHandler, bucketRequestTimes)))
	http.HandleFunc("/render/", httputil.TrackConnections(httputil.TimeHandler(renderHandler, bucketRequestTimes)))
	http.HandleFunc("/lb_check", lbCheckHandler)

	// nothing in the config? check the environment
	if Config.GraphiteHost == "" {
		if host := os.Getenv("GRAPHITEHOST") + ":" + os.Getenv("GRAPHITEPORT"); host != ":" {
			Config.GraphiteHost = host
		}
	}

	// only register g2g if we have a graphite host
	if Config.GraphiteHost != "" {

		logger.Logln("Using graphite host", Config.GraphiteHost)

		// register our metrics with graphite
		graphite, err := g2g.NewGraphite(Config.GraphiteHost, 60*time.Second, 10*time.Second)
		if err != nil {
			log.Fatal("unable to connect to to graphite: ", Config.GraphiteHost, ":", err)
		}

		hostname, _ := os.Hostname()
		hostname = strings.Replace(hostname, ".", "_", -1)

		graphite.Register(fmt.Sprintf("carbon.zipper.%s.find_requests", hostname), Metrics.FindRequests)
		graphite.Register(fmt.Sprintf("carbon.zipper.%s.find_errors", hostname), Metrics.FindErrors)

		graphite.Register(fmt.Sprintf("carbon.zipper.%s.render_requests", hostname), Metrics.RenderRequests)
		graphite.Register(fmt.Sprintf("carbon.zipper.%s.render_errors", hostname), Metrics.RenderErrors)

		graphite.Register(fmt.Sprintf("carbon.zipper.%s.timeouts", hostname), Metrics.Timeouts)

		for i := 0; i <= Config.Buckets; i++ {
			graphite.Register(fmt.Sprintf("carbon.zipper.%s.requests_in_%dms_to_%dms", hostname, i*100, (i+1)*100), bucketEntry(i))
		}
	}

	// configure the storage client
	storageClient.Transport = &http.Transport{
		MaxIdleConnsPerHost: Config.MaxIdleConnsPerHost,
	}

	portStr := fmt.Sprintf(":%d", Config.Port)
	logger.Logln("listening on", portStr)
	log.Fatal(http.ListenAndServe(portStr, nil))
}

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

// trivial logging classes

type logLevel int

const (
	LOG_NORMAL logLevel = iota
	LOG_DEBUG
	LOG_TRACE
)

// Logger is something that can log
type Logger interface {
	Log(string)
}

type stdoutLogger struct{ logger *log.Logger }

func (l *stdoutLogger) Log(s string) { l.logger.Print(s) }

type sysLogger struct{ w *syslog.Writer }

func (l *sysLogger) Log(s string) { l.w.Info(s) }

type multilog struct {
	level   logLevel
	loggers []Logger
}

func (ml *multilog) Debugf(format string, a ...interface{}) {
	if ml.level >= LOG_DEBUG {
		ml.Logf(format, a...)
	}
}

func (ml *multilog) Debugln(a ...interface{}) {
	if ml.level >= LOG_DEBUG {
		ml.Logln(a...)
	}
}

func (ml *multilog) Tracef(format string, a ...interface{}) {
	if ml.level >= LOG_TRACE {
		ml.Logf(format, a...)
	}
}

func (ml *multilog) Traceln(a ...interface{}) {
	if ml.level >= LOG_TRACE {
		ml.Logln(a...)
	}
}
func (ml *multilog) Logln(a ...interface{}) {
	s := fmt.Sprintln(a...)
	for _, l := range ml.loggers {
		l.Log(s)
	}
}

func (ml *multilog) Logf(format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	for _, l := range ml.loggers {
		l.Log(s)
	}
}

type serverLimiter map[string]chan struct{}

func newServerLimiter(servers []string, l int) serverLimiter {
	sl := make(map[string]chan struct{})

	for _, s := range servers {
		sl[s] = make(chan struct{}, l)
	}

	return sl
}

func (sl serverLimiter) enter(s string) {
	if sl == nil {
		return
	}
	sl[s] <- struct{}{}
}

func (sl serverLimiter) leave(s string) {
	if sl == nil {
		return
	}
	<-sl[s]
}
