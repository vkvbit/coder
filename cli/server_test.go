package cli_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/coder/coder/cli"
	"github.com/coder/coder/cli/clitest"
	"github.com/coder/coder/cli/config"
	"github.com/coder/coder/coderd/coderdtest"
	"github.com/coder/coder/coderd/database/postgres"
	"github.com/coder/coder/coderd/telemetry"
	"github.com/coder/coder/codersdk"
	"github.com/coder/coder/pty/ptytest"
	"github.com/coder/coder/testutil"
)

func TestReadGitAuthProvidersFromEnv(t *testing.T) {
	t.Parallel()
	t.Run("Empty", func(t *testing.T) {
		t.Parallel()
		providers, err := cli.ReadGitAuthProvidersFromEnv([]string{
			"HOME=/home/frodo",
		})
		require.NoError(t, err)
		require.Empty(t, providers)
	})
	t.Run("InvalidKey", func(t *testing.T) {
		t.Parallel()
		providers, err := cli.ReadGitAuthProvidersFromEnv([]string{
			"CODER_GITAUTH_XXX=invalid",
		})
		require.Error(t, err, "providers: %+v", providers)
		require.Empty(t, providers)
	})
	t.Run("SkipKey", func(t *testing.T) {
		t.Parallel()
		providers, err := cli.ReadGitAuthProvidersFromEnv([]string{
			"CODER_GITAUTH_0_ID=invalid",
			"CODER_GITAUTH_2_ID=invalid",
		})
		require.Error(t, err, "%+v", providers)
		require.Empty(t, providers)
	})
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		providers, err := cli.ReadGitAuthProvidersFromEnv([]string{
			"CODER_GITAUTH_0_ID=1",
			"CODER_GITAUTH_0_TYPE=gitlab",
			"CODER_GITAUTH_1_ID=2",
			"CODER_GITAUTH_1_CLIENT_ID=sid",
			"CODER_GITAUTH_1_CLIENT_SECRET=hunter12",
			"CODER_GITAUTH_1_TOKEN_URL=google.com",
			"CODER_GITAUTH_1_VALIDATE_URL=bing.com",
			"CODER_GITAUTH_1_SCOPES=repo:read repo:write",
		})
		require.NoError(t, err)
		require.Len(t, providers, 2)

		// Validate the first provider.
		assert.Equal(t, "1", providers[0].ID)
		assert.Equal(t, "gitlab", providers[0].Type)

		// Validate the second provider.
		assert.Equal(t, "2", providers[1].ID)
		assert.Equal(t, "sid", providers[1].ClientID)
		assert.Equal(t, "hunter12", providers[1].ClientSecret)
		assert.Equal(t, "google.com", providers[1].TokenURL)
		assert.Equal(t, "bing.com", providers[1].ValidateURL)
		assert.Equal(t, []string{"repo:read", "repo:write"}, providers[1].Scopes)
	})
}

// This cannot be ran in parallel because it uses a signal.
// nolint:tparallel,paralleltest
func TestServer(t *testing.T) {
	t.Run("Production", func(t *testing.T) {
		if runtime.GOOS != "linux" || testing.Short() {
			// Skip on non-Linux because it spawns a PostgreSQL instance.
			t.SkipNow()
		}
		connectionURL, closeFunc, err := postgres.Open()
		require.NoError(t, err)
		defer closeFunc()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--postgres-url", connectionURL,
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		root.Stderr = pty.Output()
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()
		accessURL := waitAccessURL(t, cfg)
		client := codersdk.New(accessURL)

		_, err = client.CreateFirstUser(ctx, coderdtest.FirstUserParams)
		require.NoError(t, err)
		cancelFunc()
		require.NoError(t, <-errC)
	})
	t.Run("BuiltinPostgres", func(t *testing.T) {
		t.Parallel()
		if testing.Short() {
			t.SkipNow()
		}
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		root.Stderr = pty.Output()
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()
		//nolint:gocritic // Embedded postgres take a while to fire up.
		require.Eventually(t, func() bool {
			rawURL, err := cfg.URL().Read()
			return err == nil && rawURL != ""
		}, 3*time.Minute, testutil.IntervalFast, "failed to get access URL")
		cancelFunc()
		require.NoError(t, <-errC)
	})
	t.Run("BuiltinPostgresURL", func(t *testing.T) {
		t.Parallel()
		root, _ := clitest.New(t, "server", "postgres-builtin-url")
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		err := root.Run()
		require.NoError(t, err)

		pty.ExpectMatch("psql")
	})
	t.Run("BuiltinPostgresURLRaw", func(t *testing.T) {
		t.Parallel()
		ctx, _ := testutil.Context(t)

		root, _ := clitest.New(t, "server", "postgres-builtin-url", "--raw-url")
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		err := root.WithContext(ctx).Run()
		require.NoError(t, err)

		got := pty.ReadLine(ctx)
		if !strings.HasPrefix(got, "postgres://") {
			t.Fatalf("expected postgres URL to start with \"postgres://\", got %q", got)
		}
	})

	// Validate that a warning is printed that it may not be externally
	// reachable.
	t.Run("LocalAccessURL", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://localhost:3000/",
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdin = pty.Input()
		root.Stdout = pty.Output()
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()

		// Just wait for startup
		_ = waitAccessURL(t, cfg)

		pty.ExpectMatch("this may cause unexpected problems when creating workspaces")
		pty.ExpectMatch("View the Web UI: http://localhost:3000/")

		cancelFunc()
		require.NoError(t, <-errC)
	})

	// Validate that an https scheme is prepended to a remote access URL
	// and that a warning is printed for a host that cannot be resolved.
	t.Run("RemoteAccessURL", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "https://foobarbaz.mydomain",
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdin = pty.Input()
		root.Stdout = pty.Output()
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()

		// Just wait for startup
		_ = waitAccessURL(t, cfg)

		pty.ExpectMatch("this may cause unexpected problems when creating workspaces")
		pty.ExpectMatch("View the Web UI: https://foobarbaz.mydomain")

		cancelFunc()
		require.NoError(t, <-errC)
	})

	t.Run("NoWarningWithRemoteAccessURL", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "https://google.com",
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdin = pty.Input()
		root.Stdout = pty.Output()
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()

		// Just wait for startup
		_ = waitAccessURL(t, cfg)

		pty.ExpectMatch("View the Web UI: https://google.com")

		cancelFunc()
		require.NoError(t, <-errC)
	})

	t.Run("NoSchemeAccessURL", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "google.com",
			"--cache-dir", t.TempDir(),
		)
		err := root.WithContext(ctx).Run()
		require.Error(t, err)
	})

	t.Run("TLSBadVersion", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "",
			"--access-url", "http://example.com",
			"--tls-enable",
			"--tls-address", ":0",
			"--tls-min-version", "tls9",
			"--cache-dir", t.TempDir(),
		)
		err := root.WithContext(ctx).Run()
		require.Error(t, err)
	})
	t.Run("TLSBadClientAuth", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "",
			"--access-url", "http://example.com",
			"--tls-enable",
			"--tls-address", ":0",
			"--tls-client-auth", "something",
			"--cache-dir", t.TempDir(),
		)
		err := root.WithContext(ctx).Run()
		require.Error(t, err)
	})
	t.Run("TLSInvalid", func(t *testing.T) {
		t.Parallel()

		cert1Path, key1Path := generateTLSCertificate(t)
		cert2Path, key2Path := generateTLSCertificate(t)

		cases := []struct {
			name        string
			args        []string
			errContains string
		}{
			{
				name:        "NoCert",
				args:        []string{"--tls-enable", "--tls-key-file", key1Path},
				errContains: "--tls-cert-file and --tls-key-file must be used the same amount of times",
			},
			{
				name:        "NoKey",
				args:        []string{"--tls-enable", "--tls-cert-file", cert1Path},
				errContains: "--tls-cert-file and --tls-key-file must be used the same amount of times",
			},
			{
				name:        "MismatchedCount",
				args:        []string{"--tls-enable", "--tls-cert-file", cert1Path, "--tls-key-file", key1Path, "--tls-cert-file", cert2Path},
				errContains: "--tls-cert-file and --tls-key-file must be used the same amount of times",
			},
			{
				name:        "MismatchedCertAndKey",
				args:        []string{"--tls-enable", "--tls-cert-file", cert1Path, "--tls-key-file", key2Path},
				errContains: "load TLS key pair",
			},
		}

		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancelFunc := context.WithCancel(context.Background())
				defer cancelFunc()

				args := []string{
					"server",
					"--in-memory",
					"--http-address", ":0",
					"--access-url", "http://example.com",
					"--cache-dir", t.TempDir(),
				}
				args = append(args, c.args...)
				root, _ := clitest.New(t, args...)
				err := root.WithContext(ctx).Run()
				require.Error(t, err)
				t.Logf("args: %v", args)
				require.ErrorContains(t, err, c.errContains)
			})
		}
	})
	t.Run("TLSValid", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		certPath, keyPath := generateTLSCertificate(t)
		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "",
			"--access-url", "https://example.com",
			"--tls-enable",
			"--tls-address", ":0",
			"--tls-cert-file", certPath,
			"--tls-key-file", keyPath,
			"--cache-dir", t.TempDir(),
		)
		clitest.Start(t, root.WithContext(ctx))

		// Verify HTTPS
		accessURL := waitAccessURL(t, cfg)
		require.Equal(t, "https", accessURL.Scheme)
		client := codersdk.New(accessURL)
		client.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					//nolint:gosec
					InsecureSkipVerify: true,
				},
			},
		}
		defer client.HTTPClient.CloseIdleConnections()
		_, err := client.HasFirstUser(ctx)
		require.NoError(t, err)
	})
	t.Run("TLSValidMultiple", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		cert1Path, key1Path := generateTLSCertificate(t, "alpaca.com")
		cert2Path, key2Path := generateTLSCertificate(t, "*.llama.com")
		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "",
			"--access-url", "https://example.com",
			"--tls-enable",
			"--tls-address", ":0",
			"--tls-cert-file", cert1Path,
			"--tls-key-file", key1Path,
			"--tls-cert-file", cert2Path,
			"--tls-key-file", key2Path,
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		clitest.Start(t, root.WithContext(ctx))

		accessURL := waitAccessURL(t, cfg)
		require.Equal(t, "https", accessURL.Scheme)
		originalHost := accessURL.Host

		var (
			expectAddr string
			dials      int64
		)
		client := codersdk.New(accessURL)
		client.HTTPClient = &http.Client{
			Transport: &http.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					atomic.AddInt64(&dials, 1)
					assert.Equal(t, expectAddr, addr)

					host, _, err := net.SplitHostPort(addr)
					require.NoError(t, err)

					// Always connect to the accessURL ip:port regardless of
					// hostname.
					conn, err := tls.Dial(network, originalHost, &tls.Config{
						MinVersion: tls.VersionTLS12,
						//nolint:gosec
						InsecureSkipVerify: true,
						ServerName:         host,
					})
					if err != nil {
						return nil, err
					}

					// We can't call conn.VerifyHostname because it requires
					// that the certificates are valid, so we call
					// VerifyHostname on the first certificate instead.
					require.Len(t, conn.ConnectionState().PeerCertificates, 1)
					err = conn.ConnectionState().PeerCertificates[0].VerifyHostname(host)
					assert.NoError(t, err, "invalid cert common name")
					return conn, nil
				},
			},
		}
		defer client.HTTPClient.CloseIdleConnections()

		// Use the first certificate and hostname.
		client.URL.Host = "alpaca.com:443"
		expectAddr = "alpaca.com:443"
		_, err := client.HasFirstUser(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, atomic.LoadInt64(&dials))

		// Use the second certificate (wildcard) and hostname.
		client.URL.Host = "hi.llama.com:443"
		expectAddr = "hi.llama.com:443"
		_, err = client.HasFirstUser(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 2, atomic.LoadInt64(&dials))
	})

	t.Run("TLSAndHTTP", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		certPath, keyPath := generateTLSCertificate(t)
		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "https://example.com",
			"--tls-enable",
			"--tls-redirect-http-to-https=false",
			"--tls-address", ":0",
			"--tls-cert-file", certPath,
			"--tls-key-file", keyPath,
			"--cache-dir", t.TempDir(),
		)
		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		root.Stderr = pty.Output()

		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()

		// We can't use waitAccessURL as it will only return the HTTP URL.
		const httpLinePrefix = "Started HTTP listener at "
		pty.ExpectMatch(httpLinePrefix)
		httpLine := pty.ReadLine(ctx)
		httpAddr := strings.TrimSpace(strings.TrimPrefix(httpLine, httpLinePrefix))
		require.NotEmpty(t, httpAddr)
		const tlsLinePrefix = "Started TLS/HTTPS listener at "
		pty.ExpectMatch(tlsLinePrefix)
		tlsLine := pty.ReadLine(ctx)
		tlsAddr := strings.TrimSpace(strings.TrimPrefix(tlsLine, tlsLinePrefix))
		require.NotEmpty(t, tlsAddr)

		// Verify HTTP
		httpURL, err := url.Parse(httpAddr)
		require.NoError(t, err)
		client := codersdk.New(httpURL)
		client.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		_, err = client.HasFirstUser(ctx)
		require.NoError(t, err)

		// Verify TLS
		tlsURL, err := url.Parse(tlsAddr)
		require.NoError(t, err)
		client = codersdk.New(tlsURL)
		client.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					//nolint:gosec
					InsecureSkipVerify: true,
				},
			},
		}
		defer client.HTTPClient.CloseIdleConnections()
		_, err = client.HasFirstUser(ctx)
		require.NoError(t, err)

		cancelFunc()
		require.NoError(t, <-errC)
	})

	t.Run("TLSRedirect", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name         string
			httpListener bool
			tlsListener  bool
			redirect     bool
			accessURL    string
			requestURL   string
			// Empty string means no redirect.
			expectRedirect string
		}{
			{
				name:           "OK",
				httpListener:   true,
				tlsListener:    true,
				redirect:       true,
				accessURL:      "https://example.com",
				expectRedirect: "https://example.com",
			},
			{
				name:           "NoRedirect",
				httpListener:   true,
				tlsListener:    true,
				accessURL:      "https://example.com",
				expectRedirect: "",
			},
			{
				name:           "NoRedirectWithWildcard",
				tlsListener:    true,
				accessURL:      "https://example.com",
				requestURL:     "https://dev.example.com",
				expectRedirect: "",
				redirect:       true,
			},
			{
				name:           "NoTLSListener",
				httpListener:   true,
				tlsListener:    false,
				accessURL:      "https://example.com",
				expectRedirect: "",
			},
			{
				name:           "NoHTTPListener",
				httpListener:   false,
				tlsListener:    true,
				accessURL:      "https://example.com",
				expectRedirect: "",
			},
		}

		for _, c := range cases {
			c := c

			t.Run(c.name, func(t *testing.T) {
				t.Parallel()

				ctx, cancelFunc := context.WithCancel(context.Background())
				defer cancelFunc()

				if c.requestURL == "" {
					c.requestURL = c.accessURL
				}

				httpListenAddr := ""
				if c.httpListener {
					httpListenAddr = ":0"
				}

				certPath, keyPath := generateTLSCertificate(t)
				flags := []string{
					"server",
					"--in-memory",
					"--cache-dir", t.TempDir(),
					"--http-address", httpListenAddr,
				}
				if c.tlsListener {
					flags = append(flags,
						"--tls-enable",
						"--tls-address", ":0",
						"--tls-cert-file", certPath,
						"--tls-key-file", keyPath,
						"--wildcard-access-url", "*.example.com",
					)
				}
				if c.accessURL != "" {
					flags = append(flags, "--access-url", c.accessURL)
				}
				if c.redirect {
					flags = append(flags, "--redirect-to-access-url")
				}

				root, _ := clitest.New(t, flags...)
				pty := ptytest.New(t)
				root.Stdout = pty.Output()
				root.Stderr = pty.Output()

				errC := make(chan error, 1)
				go func() {
					errC <- root.WithContext(ctx).Run()
				}()

				var (
					httpAddr string
					tlsAddr  string
				)
				// We can't use waitAccessURL as it will only return the HTTP URL.
				if c.httpListener {
					const httpLinePrefix = "Started HTTP listener at "
					pty.ExpectMatch(httpLinePrefix)
					httpLine := pty.ReadLine(ctx)
					httpAddr = strings.TrimSpace(strings.TrimPrefix(httpLine, httpLinePrefix))
					require.NotEmpty(t, httpAddr)
				}
				if c.tlsListener {
					const tlsLinePrefix = "Started TLS/HTTPS listener at "
					pty.ExpectMatch(tlsLinePrefix)
					tlsLine := pty.ReadLine(ctx)
					tlsAddr = strings.TrimSpace(strings.TrimPrefix(tlsLine, tlsLinePrefix))
					require.NotEmpty(t, tlsAddr)
				}

				// Verify HTTP redirects (or not)
				if c.httpListener {
					httpURL, err := url.Parse(httpAddr)
					require.NoError(t, err)
					client := codersdk.New(httpURL)
					client.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
						return http.ErrUseLastResponse
					}
					resp, err := client.Request(ctx, http.MethodGet, "/api/v2/buildinfo", nil)
					require.NoError(t, err)
					defer resp.Body.Close()
					if c.expectRedirect == "" {
						require.Equal(t, http.StatusOK, resp.StatusCode)
					} else {
						require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
						require.Equal(t, c.expectRedirect, resp.Header.Get("Location"))
					}
				}

				// Verify TLS
				if c.tlsListener {
					accessURLParsed, err := url.Parse(c.requestURL)
					require.NoError(t, err)
					client := codersdk.New(accessURLParsed)
					client.HTTPClient = &http.Client{
						CheckRedirect: func(req *http.Request, via []*http.Request) error {
							return http.ErrUseLastResponse
						},
						Transport: &http.Transport{
							DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
								return tls.Dial(network, strings.TrimPrefix(tlsAddr, "https://"), &tls.Config{
									// nolint:gosec
									InsecureSkipVerify: true,
								})
							},
						},
					}
					defer client.HTTPClient.CloseIdleConnections()
					_, err = client.HasFirstUser(ctx)
					if err != nil {
						require.ErrorContains(t, err, "Invalid application URL")
					}
					cancelFunc()
					require.NoError(t, <-errC)
				}
			})
		}
	})

	t.Run("CanListenUnspecifiedv4", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "0.0.0.0:0",
			"--access-url", "http://example.com",
		)

		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		root.Stderr = pty.Output()
		serverStop := make(chan error, 1)
		go func() {
			err := root.WithContext(ctx).Run()
			if err != nil {
				t.Error(err)
			}
			close(serverStop)
		}()

		pty.ExpectMatch("Started HTTP listener at http://0.0.0.0:")

		cancelFunc()
		<-serverStop
	})

	t.Run("CanListenUnspecifiedv6", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", "[::]:0",
			"--access-url", "http://example.com",
		)

		pty := ptytest.New(t)
		root.Stdout = pty.Output()
		root.Stderr = pty.Output()
		serverClose := make(chan struct{}, 1)
		go func() {
			err := root.WithContext(ctx).Run()
			if err != nil {
				t.Error(err)
			}
			close(serverClose)
		}()

		pty.ExpectMatch("Started HTTP listener at http://[::]:")

		cancelFunc()
		<-serverClose
	})

	t.Run("NoAddress", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":80",
			"--tls-enable=false",
			"--tls-address", "",
		)
		err := root.WithContext(ctx).Run()
		require.Error(t, err)
		require.ErrorContains(t, err, "tls-address")
	})

	t.Run("NoTLSAddress", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--tls-enable=true",
			"--tls-address", "",
		)
		err := root.WithContext(ctx).Run()
		require.Error(t, err)
		require.ErrorContains(t, err, "must not be empty")
	})

	// DeprecatedAddress is a test for the deprecated --address flag. If
	// specified, --http-address and --tls-address are both ignored, a warning
	// is printed, and the server will either be HTTP-only or TLS-only depending
	// on if --tls-enable is set.
	t.Run("DeprecatedAddress", func(t *testing.T) {
		t.Parallel()

		t.Run("HTTP", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithCancel(context.Background())
			defer cancelFunc()

			root, cfg := clitest.New(t,
				"server",
				"--in-memory",
				"--address", ":0",
				"--access-url", "http://example.com",
				"--cache-dir", t.TempDir(),
			)
			pty := ptytest.New(t)
			root.Stdout = pty.Output()
			root.Stderr = pty.Output()
			clitest.Start(t, root.WithContext(ctx))

			pty.ExpectMatch("is deprecated")

			accessURL := waitAccessURL(t, cfg)
			require.Equal(t, "http", accessURL.Scheme)
			client := codersdk.New(accessURL)
			_, err := client.HasFirstUser(ctx)
			require.NoError(t, err)
		})

		t.Run("TLS", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithCancel(context.Background())
			defer cancelFunc()

			certPath, keyPath := generateTLSCertificate(t)
			root, cfg := clitest.New(t,
				"server",
				"--in-memory",
				"--address", ":0",
				"--access-url", "https://example.com",
				"--tls-enable",
				"--tls-cert-file", certPath,
				"--tls-key-file", keyPath,
				"--cache-dir", t.TempDir(),
			)
			pty := ptytest.New(t)
			root.Stdout = pty.Output()
			root.Stderr = pty.Output()
			clitest.Start(t, root.WithContext(ctx))

			pty.ExpectMatch("is deprecated")

			accessURL := waitAccessURL(t, cfg)
			require.Equal(t, "https", accessURL.Scheme)
			client := codersdk.New(accessURL)
			client.HTTPClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						//nolint:gosec
						InsecureSkipVerify: true,
					},
				},
			}
			defer client.HTTPClient.CloseIdleConnections()
			_, err := client.HasFirstUser(ctx)
			require.NoError(t, err)
		})
	})

	// This cannot be ran in parallel because it uses a signal.
	//nolint:paralleltest
	t.Run("Shutdown", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			// Sending interrupt signal isn't supported on Windows!
			t.SkipNow()
		}
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--provisioner-daemons", "1",
			"--cache-dir", t.TempDir(),
		)
		serverErr := make(chan error, 1)
		go func() {
			serverErr <- root.WithContext(ctx).Run()
		}()
		_ = waitAccessURL(t, cfg)
		currentProcess, err := os.FindProcess(os.Getpid())
		require.NoError(t, err)
		err = currentProcess.Signal(os.Interrupt)
		require.NoError(t, err)
		// We cannot send more signals here, because it's possible Coder
		// has already exited, which could cause the test to fail due to interrupt.
		err = <-serverErr
		require.NoError(t, err)
	})
	t.Run("TracerNoLeak", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--trace=true",
			"--cache-dir", t.TempDir(),
		)
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()
		cancelFunc()
		require.NoError(t, <-errC)
		require.Error(t, goleak.Find())
	})
	t.Run("Telemetry", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		deployment := make(chan struct{}, 64)
		snapshot := make(chan *telemetry.Snapshot, 64)
		r := chi.NewRouter()
		r.Post("/deployment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			deployment <- struct{}{}
		})
		r.Post("/snapshot", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			ss := &telemetry.Snapshot{}
			err := json.NewDecoder(r.Body).Decode(ss)
			require.NoError(t, err)
			snapshot <- ss
		})
		server := httptest.NewServer(r)
		defer server.Close()

		root, _ := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--telemetry",
			"--telemetry-url", server.URL,
			"--cache-dir", t.TempDir(),
		)
		errC := make(chan error, 1)
		go func() {
			errC <- root.WithContext(ctx).Run()
		}()

		<-deployment
		<-snapshot
		cancelFunc()
		<-errC
	})
	t.Run("Prometheus", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		random, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		_ = random.Close()
		tcpAddr, valid := random.Addr().(*net.TCPAddr)
		require.True(t, valid)
		randomPort := tcpAddr.Port

		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--provisioner-daemons", "1",
			"--prometheus-enable",
			"--prometheus-address", ":"+strconv.Itoa(randomPort),
			"--cache-dir", t.TempDir(),
		)
		serverErr := make(chan error, 1)
		go func() {
			serverErr <- root.WithContext(ctx).Run()
		}()
		_ = waitAccessURL(t, cfg)

		var res *http.Response
		require.Eventually(t, func() bool {
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d", randomPort), nil)
			assert.NoError(t, err)
			// nolint:bodyclose
			res, err = http.DefaultClient.Do(req)
			return err == nil
		}, testutil.WaitShort, testutil.IntervalFast)

		scanner := bufio.NewScanner(res.Body)
		hasActiveUsers := false
		hasWorkspaces := false
		for scanner.Scan() {
			// This metric is manually registered to be tracked in the server. That's
			// why we test it's tracked here.
			if strings.HasPrefix(scanner.Text(), "coderd_api_active_users_duration_hour") {
				hasActiveUsers = true
				continue
			}
			if strings.HasPrefix(scanner.Text(), "coderd_api_workspace_latest_build_total") {
				hasWorkspaces = true
				continue
			}
			t.Logf("scanned %s", scanner.Text())
		}
		require.NoError(t, scanner.Err())
		require.True(t, hasActiveUsers)
		require.True(t, hasWorkspaces)
		cancelFunc()
		<-serverErr
	})
	t.Run("GitHubOAuth", func(t *testing.T) {
		t.Parallel()
		ctx, cancelFunc := context.WithCancel(context.Background())
		defer cancelFunc()

		fakeRedirect := "https://fake-url.com"
		root, cfg := clitest.New(t,
			"server",
			"--in-memory",
			"--http-address", ":0",
			"--access-url", "http://example.com",
			"--oauth2-github-allow-everyone",
			"--oauth2-github-client-id", "fake",
			"--oauth2-github-client-secret", "fake",
			"--oauth2-github-enterprise-base-url", fakeRedirect,
		)
		serverErr := make(chan error, 1)
		go func() {
			serverErr <- root.WithContext(ctx).Run()
		}()
		accessURL := waitAccessURL(t, cfg)
		client := codersdk.New(accessURL)
		client.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		githubURL, err := accessURL.Parse("/api/v2/users/oauth2/github")
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubURL.String(), nil)
		require.NoError(t, err)
		res, err := client.HTTPClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		fakeURL, err := res.Location()
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(fakeURL.String(), fakeRedirect), fakeURL.String())
		cancelFunc()
		<-serverErr
	})

	t.Run("RateLimit", func(t *testing.T) {
		t.Parallel()

		t.Run("Default", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithCancel(context.Background())
			defer cancelFunc()

			root, cfg := clitest.New(t,
				"server",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
			)
			serverErr := make(chan error, 1)
			go func() {
				serverErr <- root.WithContext(ctx).Run()
			}()
			accessURL := waitAccessURL(t, cfg)
			client := codersdk.New(accessURL)

			resp, err := client.Request(ctx, http.MethodGet, "/api/v2/buildinfo", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "512", resp.Header.Get("X-Ratelimit-Limit"))
			cancelFunc()
			<-serverErr
		})

		t.Run("Changed", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithCancel(context.Background())
			defer cancelFunc()

			val := "100"
			root, cfg := clitest.New(t,
				"server",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--api-rate-limit", val,
			)
			serverErr := make(chan error, 1)
			go func() {
				serverErr <- root.WithContext(ctx).Run()
			}()
			accessURL := waitAccessURL(t, cfg)
			client := codersdk.New(accessURL)

			resp, err := client.Request(ctx, http.MethodGet, "/api/v2/buildinfo", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, val, resp.Header.Get("X-Ratelimit-Limit"))
			cancelFunc()
			<-serverErr
		})

		t.Run("Disabled", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithCancel(context.Background())
			defer cancelFunc()

			root, cfg := clitest.New(t,
				"server",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--api-rate-limit", "-1",
			)
			serverErr := make(chan error, 1)
			go func() {
				serverErr <- root.WithContext(ctx).Run()
			}()
			accessURL := waitAccessURL(t, cfg)
			client := codersdk.New(accessURL)

			resp, err := client.Request(ctx, http.MethodGet, "/api/v2/buildinfo", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "", resp.Header.Get("X-Ratelimit-Limit"))
			cancelFunc()
			<-serverErr
		})
	})

	waitFile := func(t *testing.T, fiName string, dur time.Duration) {
		var lastStat os.FileInfo
		require.Eventually(t, func() bool {
			var err error
			lastStat, err = os.Stat(fiName)
			if err != nil {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected error: %v", err)
				}
				return false
			}
			return lastStat.Size() > 0
		},
			testutil.WaitShort,
			testutil.IntervalFast,
			"file at %s should exist, last stat: %+v",
			fiName, lastStat,
		)
	}

	t.Run("Logging", func(t *testing.T) {
		t.Parallel()

		t.Run("CreatesFile", func(t *testing.T) {
			t.Parallel()
			fiName := testutil.TempFile(t, "", "coder-logging-test-*")

			root, _ := clitest.New(t,
				"server",
				"--verbose",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--log-human", fiName,
			)
			clitest.Start(t, root)

			waitFile(t, fiName, testutil.WaitShort)
		})

		t.Run("Human", func(t *testing.T) {
			t.Parallel()
			fi := testutil.TempFile(t, "", "coder-logging-test-*")

			root, _ := clitest.New(t,
				"server",
				"--verbose",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--log-human", fi,
			)
			clitest.Start(t, root)

			waitFile(t, fi, testutil.WaitShort)
		})

		t.Run("JSON", func(t *testing.T) {
			t.Parallel()
			fi := testutil.TempFile(t, "", "coder-logging-test-*")

			root, _ := clitest.New(t,
				"server",
				"--verbose",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--log-json", fi,
			)
			clitest.Start(t, root)

			waitFile(t, fi, testutil.WaitShort)
		})

		t.Run("Stackdriver", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
			defer cancelFunc()

			fi := testutil.TempFile(t, "", "coder-logging-test-*")

			root, _ := clitest.New(t,
				"server",
				"--verbose",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--log-stackdriver", fi,
			)
			// Attach pty so we get debug output from the command if this test
			// fails.
			pty := ptytest.New(t)
			root.Stdout = pty.Output()
			root.Stderr = pty.Output()

			serverErr := make(chan error, 1)
			go func() {
				serverErr <- root.WithContext(ctx).Run()
			}()
			defer func() {
				cancelFunc()
				<-serverErr
			}()

			// Wait for server to listen on HTTP, this is a good
			// starting point for expecting logs.
			_ = pty.ExpectMatchContext(ctx, "Started HTTP listener at ")

			waitFile(t, fi, testutil.WaitSuperLong)
		})

		t.Run("Multiple", func(t *testing.T) {
			t.Parallel()
			ctx, cancelFunc := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
			defer cancelFunc()

			fi1 := testutil.TempFile(t, "", "coder-logging-test-*")
			fi2 := testutil.TempFile(t, "", "coder-logging-test-*")
			fi3 := testutil.TempFile(t, "", "coder-logging-test-*")

			// NOTE(mafredri): This test might end up downloading Terraform
			// which can take a long time and end up failing the test.
			// This is why we wait extra long below for server to listen on
			// HTTP.
			inv, _ := clitest.New(t,
				"server",
				"--verbose",
				"--in-memory",
				"--http-address", ":0",
				"--access-url", "http://example.com",
				"--log-human", fi1,
				"--log-json", fi2,
				"--log-stackdriver", fi3,
			)
			// Attach pty so we get debug output from the command if this test
			// fails.
			pty := ptytest.New(t)
			inv.Stdout = pty.Output()
			inv.Stderr = pty.Output()

			clitest.Start(t, inv)

			// Wait for server to listen on HTTP, this is a good
			// starting point for expecting logs.
			_ = pty.ExpectMatchContext(ctx, "Started HTTP listener at ")

			waitFile(t, fi1, testutil.WaitSuperLong)
			waitFile(t, fi2, testutil.WaitSuperLong)
			waitFile(t, fi3, testutil.WaitSuperLong)
		})
	})
}

func generateTLSCertificate(t testing.TB, commonName ...string) (certPath, keyPath string) {
	dir := t.TempDir()

	commonNameStr := "localhost"
	if len(commonName) > 0 {
		commonNameStr = commonName[0]
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
			CommonName:   commonNameStr,
		},
		DNSNames:  []string{commonNameStr},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(time.Hour * 24 * 180),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)
	certFile, err := os.CreateTemp(dir, "")
	require.NoError(t, err)
	defer certFile.Close()
	_, err = certFile.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
	require.NoError(t, err)
	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	keyFile, err := os.CreateTemp(dir, "")
	require.NoError(t, err)
	defer keyFile.Close()
	err = pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyBytes})
	require.NoError(t, err)
	return certFile.Name(), keyFile.Name()
}

func waitAccessURL(t *testing.T, cfg config.Root) *url.URL {
	t.Helper()

	var err error
	var rawURL string
	require.Eventually(t, func() bool {
		rawURL, err = cfg.URL().Read()
		return err == nil && rawURL != ""
	}, testutil.WaitLong, testutil.IntervalFast, "failed to get access URL")

	accessURL, err := url.Parse(rawURL)
	require.NoError(t, err, "failed to parse access URL")

	return accessURL
}
