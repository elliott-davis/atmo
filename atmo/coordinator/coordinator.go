package coordinator

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/pkg/errors"
	"github.com/suborbital/atmo/atmo/appsource"
	"github.com/suborbital/atmo/atmo/options"
	"github.com/suborbital/atmo/directive"
	"github.com/suborbital/grav/discovery/local"
	"github.com/suborbital/grav/grav"
	"github.com/suborbital/grav/transport/websocket"
	"github.com/suborbital/reactr/rt"
	"github.com/suborbital/vektor/vk"
	"github.com/suborbital/vektor/vlog"
)

const (
	atmoMethodSchedule       = "SCHED"
	atmoHeadlessStateHeader  = "X-Atmo-State"
	atmoHeadlessParamsHeader = "X-Atmo-Params"
	atmoRequestIDHeader      = "X-Atmo-RequestID"
	atmoMessageURI           = "/meta/message"
)

type rtFunc func(rt.Job, *rt.Ctx) (interface{}, error)

// Coordinator is a type that is responsible for covnerting the directive into
// usable Vektor handles by coordinating Reactr jobs and meshing when needed.
type Coordinator struct {
	App  appsource.AppSource
	opts *options.Options

	log *vlog.Logger

	reactr *rt.Reactr

	grav      *grav.Grav
	transport *websocket.Transport

	listening sync.Map
}

type requestScope struct {
	RequestID string `json:"request_id"`
}

// New creates a coordinator
func New(appSource appsource.AppSource, options *options.Options) *Coordinator {
	reactr := rt.New()

	gravOpts := []grav.OptionsModifier{
		grav.UseLogger(options.Logger),
	}

	var transport *websocket.Transport

	if options.ControlPlane != "" {
		transport = websocket.New()
		d := local.New()

		gravOpts = append(gravOpts, grav.UseTransport(transport))
		gravOpts = append(gravOpts, grav.UseDiscovery(d))
	}

	grav := grav.New(gravOpts...)

	c := &Coordinator{
		App:       appSource,
		opts:      options,
		log:       options.Logger,
		reactr:    reactr,
		grav:      grav,
		transport: transport,
		listening: sync.Map{},
	}

	return c
}

// Start allows the Coordinator to bootstrap
func (c *Coordinator) Start() error {
	if err := c.App.Start(*c.opts); err != nil {
		return errors.Wrap(err, "failed to App.Start")
	}

	// do an initial sync of Runnables
	// from the AppSource into RVG
	c.SyncAppState()

	return nil
}

// GenerateRouter generates a Vektor Router for the app
func (c *Coordinator) GenerateRouter() *vk.Router {
	router := vk.NewRouter(c.log)

	// set a middleware on the root RouteGroup
	router.Before(scopeMiddleware)

	// mount each handler into the VK group
	for _, h := range c.App.Handlers() {
		switch h.Input.Type {
		case directive.InputTypeRequest:
			router.Handle(h.Input.Method, h.Input.Resource, c.vkHandlerForDirectiveHandler(h))
		case directive.InputTypeStream:
			router.HandleHTTP(http.MethodGet, h.Input.Resource, c.websocketHandlerForDirectiveHandler(h))
		}
	}

	if c.transport != nil {
		router.HandleHTTP(http.MethodGet, atmoMessageURI, c.transport.HTTPHandlerFunc())
	}

	return router
}

func (c *Coordinator) SetSchedules() {
	// mount each schedule into Reactr
	for _, s := range c.App.Schedules() {
		rtFunc := c.rtFuncForDirectiveSchedule(s)

		// create basically an fqfn for this schedule (com.suborbital.appname#schedule.dojob@v0.1.0)
		jobName := fmt.Sprintf("%s#schedule.%s@%s", c.App.Meta().Identifier, s.Name, c.App.Meta().AppVersion)

		c.reactr.Register(jobName, &scheduledRunner{rtFunc})

		seconds := s.NumberOfSeconds()

		// only actually schedule the job if the env var isn't set (or is set but not 'false')
		// the job stays mounted on reactr because we could get a request to run it from grav
		if *c.opts.RunSchedules {
			c.log.Debug("adding schedule", jobName)

			c.reactr.Schedule(rt.Every(seconds, func() rt.Job {
				return rt.NewJob(jobName, nil)
			}))
		}
	}
}

// resultFromState returns the state value for the last single function that ran in a handler
func resultFromState(handler directive.Handler, state map[string][]byte) []byte {
	// if the handler defines a response explicitly, use it (return nil if there is nothing in state)
	if handler.Response != "" {
		resp, exists := state[handler.Response]
		if exists {
			return resp
		}

		return nil
	}

	// if not, use the last step. If last step is a group, return nil
	step := handler.Steps[len(handler.Steps)-1]
	if step.IsGroup() {
		return nil
	}

	// determine what the state key is
	key := step.Fn
	if step.IsForEach() {
		key = step.ForEach.As
	}

	val, exists := state[key]
	if exists {
		return val
	}

	return nil
}

func scopeMiddleware(r *http.Request, ctx *vk.Ctx) error {
	scope := requestScope{
		RequestID: ctx.RequestID(),
	}

	ctx.UseScope(scope)

	return nil
}
