package externalproxy

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/coder/coder/codersdk"

	"github.com/coder/coder/buildinfo"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/coder/coder/coderd/tracing"
	"go.opentelemetry.io/otel/trace"

	"github.com/go-chi/chi/v5"

	"github.com/coder/coder/coderd/wsconncache"

	"github.com/coder/coder/coderd/httpmw"

	"cdr.dev/slog"
	"github.com/coder/coder/coderd/workspaceapps"
)

type Options struct {
	Logger slog.Logger

	// PrimaryAccessURL is the URL of the primary coderd instance.
	// This also serves as the DashboardURL.
	PrimaryAccessURL *url.URL
	// AccessURL is the URL of the WorkspaceProxy. This is the url to communicate
	// with this server.
	AccessURL *url.URL

	// TODO: @emyrk We use these two fields in many places with this comment.
	//		Maybe we should make some shared options struct?
	// AppHostname should be the wildcard hostname to use for workspace
	// applications INCLUDING the asterisk, (optional) suffix and leading dot.
	// It will use the same scheme and port number as the access URL.
	// E.g. "*.apps.coder.com" or "*-apps.coder.com".
	AppHostname string
	// AppHostnameRegex contains the regex version of options.AppHostname as
	// generated by httpapi.CompileHostnamePattern(). It MUST be set if
	// options.AppHostname is set.
	AppHostnameRegex *regexp.Regexp

	RealIPConfig *httpmw.RealIPConfig
	// TODO: @emyrk this key needs to be provided via a file or something?
	//		Maybe we should curl it from the primary over some secure connection?
	AppSecurityKey workspaceapps.SecurityKey

	Tracing            trace.TracerProvider
	PrometheusRegistry *prometheus.Registry

	APIRateLimit     int
	SecureAuthCookie bool
}

// Server is an external workspace proxy server. This server can communicate
// directly with a workspace. It requires a primary coderd to establish a said
// connection.
type Server struct {
	PrimaryAccessURL *url.URL
	AppServer        *workspaceapps.Server

	// Logging/Metrics
	Logger             slog.Logger
	TracerProvider     trace.TracerProvider
	PrometheusRegistry *prometheus.Registry

	Handler chi.Router

	// TODO: Missing:
	//		- derpserver

	Options *Options
	// SDKClient is a client to the primary coderd instance.
	// TODO: We really only need 'DialWorkspaceAgent', so maybe just pass that?
	SDKClient *codersdk.Client

	// Used for graceful shutdown.
	// Required for the dialer.
	ctx    context.Context
	cancel context.CancelFunc
}

func New(opts *Options) *Server {
	if opts.PrometheusRegistry == nil {
		opts.PrometheusRegistry = prometheus.NewRegistry()
	}

	client := codersdk.New(opts.PrimaryAccessURL)
	// TODO: @emyrk we need to implement some form of authentication for the
	// 		external proxy to the the primary. This allows us to make workspace
	//		connections.
	//		Ideally we reuse the same client as the cli, but this can be changed.
	//		If the auth fails, we need some logic to retry and make sure this client
	//		is always authenticated and usable.
	client.SetSessionToken("fake-token")

	r := chi.NewRouter()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		Options:            opts,
		PrimaryAccessURL:   opts.PrimaryAccessURL,
		Logger:             opts.Logger.Named("workspace-proxy"),
		TracerProvider:     opts.Tracing,
		PrometheusRegistry: opts.PrometheusRegistry,
		Handler:            r,
		ctx:                ctx,
		cancel:             cancel,
	}

	s.AppServer = &workspaceapps.Server{
		Logger:        opts.Logger.Named("workspaceapps"),
		DashboardURL:  opts.PrimaryAccessURL,
		AccessURL:     opts.AccessURL,
		Hostname:      opts.AppHostname,
		HostnameRegex: opts.AppHostnameRegex,
		// TODO: @emyrk We should reduce the options passed in here.
		DeploymentValues: nil,
		RealIPConfig:     opts.RealIPConfig,
		// TODO: @emyrk we need to implement this for external token providers.
		SignedTokenProvider: nil,
		WorkspaceConnCache:  wsconncache.New(s.DialWorkspaceAgent, 0),
		AppSecurityKey:      opts.AppSecurityKey,
	}

	// Routes
	apiRateLimiter := httpmw.RateLimit(opts.APIRateLimit, time.Minute)
	// Persistant middlewares to all routes
	r.Use(
		// TODO: @emyrk Should we standardize these in some other package?
		httpmw.Recover(s.Logger),
		tracing.StatusWriterMiddleware,
		tracing.Middleware(s.TracerProvider),
		httpmw.AttachRequestID,
		httpmw.ExtractRealIP(s.Options.RealIPConfig),
		httpmw.Logger(s.Logger),
		httpmw.Prometheus(s.PrometheusRegistry),

		// SubdomainAppMW is a middleware that handles all requests to the
		// subdomain based workspace apps.
		s.AppServer.SubdomainAppMW(apiRateLimiter),
		// Build-Version is helpful for debugging.
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Coder-Build-Version", buildinfo.Version())
				next.ServeHTTP(w, r)
			})
		},
		// This header stops a browser from trying to MIME-sniff the content type and
		// forces it to stick with the declared content-type. This is the only valid
		// value for this header.
		// See: https://github.com/coder/security/issues/12
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Content-Type-Options", "nosniff")
				next.ServeHTTP(w, r)
			})
		},
		// TODO: @emyrk we might not need this? But good to have if it does
		// 		not break anything.
		httpmw.CSRF(s.Options.SecureAuthCookie),
	)

	// Attach workspace apps routes.
	r.Group(func(r chi.Router) {
		r.Use(apiRateLimiter)
		s.AppServer.Attach(r)
	})

	// TODO: @emyrk Buildinfo and healthz routes.

	return s
}

func (s *Server) Close() error {
	s.cancel()
	return s.AppServer.Close()
}

func (s *Server) DialWorkspaceAgent(id uuid.UUID) (*codersdk.WorkspaceAgentConn, error) {
	return s.SDKClient.DialWorkspaceAgent(s.ctx, id, nil)
}
