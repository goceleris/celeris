package spec

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"

	"golang.org/x/net/http2"
)

// TestHTTP1Parallel sends concurrent HTTP/1.1 requests to each engine.
func TestHTTP1Parallel(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.HTTP1)
			client := &http.Client{
				Timeout: 5 * time.Second,
				Transport: &http.Transport{
					MaxIdleConnsPerHost: 50,
					MaxConnsPerHost:     50,
				},
			}

			const numRequests = 100
			var wg sync.WaitGroup
			errors := make(chan error, numRequests)

			for i := range numRequests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					path := fmt.Sprintf("/req/%d", i)
					resp, err := client.Get("http://" + addr + path)
					if err != nil {
						errors <- fmt.Errorf("request %d: %w", i, err)
						return
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						errors <- fmt.Errorf("request %d: status %d", i, resp.StatusCode)
						return
					}
					if !strings.Contains(string(body), path) {
						errors <- fmt.Errorf("request %d: body %q missing path %s", i, body, path)
					}
				}()
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}

// TestH2CParallel sends concurrent H2C requests to each engine.
func TestH2CParallel(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.H2C)
			transport := &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, a)
				},
			}
			client := &http.Client{
				Timeout:   10 * time.Second,
				Transport: transport,
			}

			const numRequests = 100
			var wg sync.WaitGroup
			errors := make(chan error, numRequests)

			for i := range numRequests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					path := fmt.Sprintf("/h2c/%d", i)
					resp, err := client.Get("http://" + addr + path)
					if err != nil {
						errors <- fmt.Errorf("request %d: %w", i, err)
						return
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						errors <- fmt.Errorf("request %d: status %d", i, resp.StatusCode)
						return
					}
					if resp.ProtoMajor != 2 {
						errors <- fmt.Errorf("request %d: proto %d.%d, want 2.x", i, resp.ProtoMajor, resp.ProtoMinor)
						return
					}
					if !strings.Contains(string(body), path) {
						errors <- fmt.Errorf("request %d: body %q missing path %s", i, body, path)
					}
				}()
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}

// TestAutoProtocolDetection starts engines in Auto mode and sends both
// HTTP/1.1 and H2C requests, verifying protocol detection works.
func TestAutoProtocolDetection(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.Auto)

			t.Run("HTTP1", func(t *testing.T) {
				client := &http.Client{Timeout: 5 * time.Second}
				resp, err := client.Get("http://" + addr + "/auto-h1")
				if err != nil {
					t.Fatalf("HTTP/1.1 request: %v", err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Errorf("HTTP/1.1: status %d", resp.StatusCode)
				}
				if !strings.Contains(string(body), "/auto-h1") {
					t.Errorf("HTTP/1.1: body %q missing path", body)
				}
			})

			t.Run("H2C", func(t *testing.T) {
				transport := &http2.Transport{
					AllowHTTP: true,
					DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, network, a)
					},
				}
				client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
				resp, err := client.Get("http://" + addr + "/auto-h2c")
				if err != nil {
					t.Fatalf("H2C request: %v", err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Errorf("H2C: status %d", resp.StatusCode)
				}
				if resp.ProtoMajor != 2 {
					t.Errorf("H2C: proto %d.%d, want 2.x", resp.ProtoMajor, resp.ProtoMinor)
				}
				if !strings.Contains(string(body), "/auto-h2c") {
					t.Errorf("H2C: body %q missing path", body)
				}
			})

			t.Run("MixedParallel", func(t *testing.T) {
				h1Client := &http.Client{
					Timeout:   5 * time.Second,
					Transport: &http.Transport{MaxConnsPerHost: 20},
				}
				h2Transport := &http2.Transport{
					AllowHTTP: true,
					DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, network, a)
					},
				}
				h2Client := &http.Client{Timeout: 5 * time.Second, Transport: h2Transport}

				const numEach = 25
				var wg sync.WaitGroup
				errors := make(chan error, numEach*2)

				for i := range numEach {
					wg.Add(2)
					go func() {
						defer wg.Done()
						resp, err := h1Client.Get(fmt.Sprintf("http://%s/mixed-h1/%d", addr, i))
						if err != nil {
							errors <- fmt.Errorf("h1 %d: %w", i, err)
							return
						}
						_, _ = io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode != 200 {
							errors <- fmt.Errorf("h1 %d: status %d", i, resp.StatusCode)
						}
					}()
					go func() {
						defer wg.Done()
						resp, err := h2Client.Get(fmt.Sprintf("http://%s/mixed-h2/%d", addr, i))
						if err != nil {
							errors <- fmt.Errorf("h2 %d: %w", i, err)
							return
						}
						_, _ = io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode != 200 {
							errors <- fmt.Errorf("h2 %d: status %d", i, resp.StatusCode)
						}
					}()
				}

				wg.Wait()
				close(errors)
				for err := range errors {
					t.Error(err)
				}
			})
		})
	}
}

// TestH2CMultipleStreams opens a single H2C connection and sends many
// requests concurrently over multiplexed streams.
func TestH2CMultipleStreams(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.H2C)
			transport := &http2.Transport{
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, a)
				},
			}
			client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

			const numStreams = 50
			var wg sync.WaitGroup
			errors := make(chan error, numStreams)

			for i := range numStreams {
				wg.Add(1)
				go func() {
					defer wg.Done()
					path := fmt.Sprintf("/stream/%d", i)
					body := strings.NewReader(fmt.Sprintf("payload-%d", i))
					resp, err := client.Post("http://"+addr+path, "text/plain", body)
					if err != nil {
						errors <- fmt.Errorf("stream %d: %w", i, err)
						return
					}
					respBody, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						errors <- fmt.Errorf("stream %d: status %d", i, resp.StatusCode)
						return
					}
					expected := fmt.Sprintf("payload-%d", i)
					if !strings.Contains(string(respBody), expected) {
						errors <- fmt.Errorf("stream %d: body %q missing %s", i, respBody, expected)
					}
				}()
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}

// TestHTTP1LargeBodyParallel sends concurrent large POST requests.
func TestHTTP1LargeBodyParallel(t *testing.T) {
	for _, se := range specEngines {
		t.Run(se.name, func(t *testing.T) {
			addr := startSpecEngine(t, se, engine.HTTP1)
			client := &http.Client{
				Timeout:   30 * time.Second,
				Transport: &http.Transport{MaxConnsPerHost: 10},
			}

			const numRequests = 10
			const bodySize = 512 * 1024 // 512KB each
			var wg sync.WaitGroup
			errors := make(chan error, numRequests)

			for i := range numRequests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					body := strings.NewReader(strings.Repeat("x", bodySize))
					resp, err := client.Post("http://"+addr+"/upload", "application/octet-stream", body)
					if err != nil {
						errors <- fmt.Errorf("upload %d: %w", i, err)
						return
					}
					_, _ = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						errors <- fmt.Errorf("upload %d: status %d", i, resp.StatusCode)
					}
				}()
			}

			wg.Wait()
			close(errors)
			for err := range errors {
				t.Error(err)
			}
		})
	}
}
