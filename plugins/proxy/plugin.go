package proxy

import (
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"

	"github.com/mojura/kiroku"
	"github.com/mojura/source-proxy/libs/apikeys"
	"github.com/mojura/source-proxy/libs/resources"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vroomy/httpserve"
	"github.com/vroomy/vroomy"
)

var p Plugin

const defaultMatch = "[0-9]+"

var errForbidden = errors.New("forbidden")

func init() {
	if err := vroomy.Register("proxy", &p); err != nil {
		log.Fatal(err)
	}
}

type Plugin struct {
	mux sync.Mutex
	vroomy.BasePlugin

	match *regexp.Regexp

	// Backend
	Source    kiroku.Source        `vroomy:"mojura-source"`
	APIKeys   *apikeys.APIKeys     `vroomy:"apikeys"`
	Resources *resources.Resources `vroomy:"resources"`

	getsStarted   prometheus.Counter
	getsCompleted prometheus.Counter
	getsErrored   prometheus.Counter

	getNextsStarted   prometheus.Counter
	getNextsCompleted prometheus.Counter
	getNextsErrored   prometheus.Counter

	exportsStarted   prometheus.Counter
	exportsCompleted prometheus.Counter
	exportsErrored   prometheus.Counter
}

// New ensures Profiles Database is built and open for access
func (p *Plugin) Load(env vroomy.Environment) (err error) {
	var (
		matchExpression string
		ok              bool
	)

	if matchExpression, ok = env["matchExpression"]; !ok {
		matchExpression = defaultMatch
	}

	if p.match, err = regexp.Compile(matchExpression); err != nil {
		err = fmt.Errorf("error compiling match expression of <%s>", matchExpression)
		return
	}

	p.getsStarted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_gets_started_total",
		Help: "The number of Get events started",
	})

	p.getsCompleted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_gets_completed_total",
		Help: "The number of Get events completed",
	})

	p.getsErrored = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_gets_errored_total",
		Help: "The number of Get events with errors",
	})

	p.getNextsStarted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_get_nexts_started_total",
		Help: "The number of GetNext events started",
	})

	p.getNextsCompleted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_get_nexts_completed_total",
		Help: "The number of GetNext events completed",
	})

	p.getNextsErrored = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_get_nexts_errored_total",
		Help: "The number of GetNext events with errors",
	})

	p.exportsStarted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_exports_started_total",
		Help: "The number of Export events started",
	})

	p.exportsCompleted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_exports_completed_total",
		Help: "The number of Export events completed",
	})

	p.exportsErrored = promauto.NewCounter(prometheus.CounterOpts{
		Name: "source_proxy_exports_errored_total",
		Help: "The number of exExportport events with errors",
	})
	return
}

// Backend exposes this plugin's data layer to other plugins
func (p *Plugin) Backend() interface{} {
	return p
}

// Ingest will ingest logs and set IDs if necessary
func (p *Plugin) Export(ctx *httpserve.Context) {
	var (
		newFilename string
		err         error
	)

	p.exportsStarted.Add(1)
	req := ctx.Request()
	prefix := ctx.Param("prefix")
	p.mux.Lock()
	filename := updateFilename(ctx.Param("filename"))
	p.mux.Unlock()

	// We need to copy the request body to a file so that the s3 library can determine the max content length
	if err = copyToTemp(req.Body, func(f *os.File) (err error) {
		if newFilename, err = p.Source.Export(req.Context(), prefix, filename, f); err != nil {
			err = fmt.Errorf("error exporting: %v", err)
			return
		}

		return
	}); err != nil {
		ctx.WriteJSON(400, err)
		p.exportsErrored.Add(1)
		return
	}

	ctx.WriteString(200, "text/plain", newFilename)
	p.exportsCompleted.Add(1)
}

// Get will get a file by name
func (p *Plugin) Get(ctx *httpserve.Context) {
	p.getsStarted.Add(1)
	req := ctx.Request()
	prefix := ctx.Param("prefix")
	filename := ctx.Param("filename")
	if err := p.Source.Import(req.Context(), prefix, filename, ctx.Writer()); err != nil {
		err = fmt.Errorf("error getting: %v", err)
		ctx.WriteJSON(400, err)
		p.getsErrored.Add(1)
		return
	}

	p.getsCompleted.Add(1)
}

// Get will get a file by name
func (p *Plugin) GetNext(ctx *httpserve.Context) {
	var (
		nextFilename string
		err          error
	)

	p.getNextsStarted.Add(1)
	req := ctx.Request()
	prefix := ctx.Param("prefix")
	lastFilename := ctx.Param("filename")
	if nextFilename, err = p.Source.GetNext(req.Context(), prefix, lastFilename); err != nil {
		err = fmt.Errorf("error getting next filename: %v", err)
		ctx.WriteJSON(400, err)
		p.getNextsErrored.Add(1)
		return
	}

	ctx.WriteJSON(200, nextFilename)
	p.getNextsCompleted.Add(1)
}

func (p *Plugin) CheckPermissionsMW(ctx *httpserve.Context) {
	var (
		apikey string
		err    error
	)

	if apikey, err = getAPIKey(ctx); err != nil {
		ctx.WriteJSON(400, err)
		return
	}

	var resource string
	prefix := ctx.Param("prefix")
	filename := ctx.Param("filename")
	if resource, err = getResource(prefix, filename); err != nil {
		ctx.WriteJSON(400, err)
		return
	}

	method := ctx.Request().Method
	groups := p.APIKeys.Groups(apikey)

	if !p.Resources.Can(method, resource, groups...) {
		fmt.Printf("forbidden request: Prefix: <%s> / Filename: <%s> / Resource <%s> / Last 4 API Key <%s>\n", prefix, filename, resource, apikey[len(apikey)-4:])
		ctx.WriteJSON(401, errForbidden)
		return
	}

	ctx.Put("resource", resource)
}
