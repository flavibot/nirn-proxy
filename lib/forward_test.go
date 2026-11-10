package lib

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

const reactionsPath = "/api/v10/channels/872712139712913438/messages/872712150509047809/reactions/"
const emojisPath = "/api/v10/guilds/103039963636301824/emojis/"

func TestMain(m *testing.M) {
	l := logrus.New()
	l.SetOutput(io.Discard)
	SetLogger(l)
	os.Exit(m.Run())
}

// recorder keeps the request line of every request a test server received.
type recorder struct {
	sync.Mutex
	seen []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.Lock()
	defer r.Unlock()
	r.seen = append(r.seen, requestLine(req.Method, req.RequestURI))
}

func (r *recorder) requests() []string {
	r.Lock()
	defer r.Unlock()
	return append([]string(nil), r.seen...)
}

// requestLine drops the bare "?" the proxy appends when there is no query.
func requestLine(method, target string) string {
	return method + " " + strings.TrimSuffix(target, "?")
}

// newProxy starts the proxy in front of a fake Discord and returns the proxy's address.
func newProxy(t *testing.T, discord http.Handler) string {
	t.Helper()

	upstream := httptest.NewTLSServer(discord)
	t.Cleanup(upstream.Close)

	ConfigureDiscordHTTPClient("", 5*time.Second, "", false, true, false)
	// Whatever the proxy addresses to discord.com lands on the fake.
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com" // the name on httptest's certificate
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client.Transport = transport

	proxy := httptest.NewServer(NewQueueManager(50, 10).CreateMux())
	t.Cleanup(proxy.Close)
	return proxy.Listener.Addr().String()
}

// send writes the request target byte for byte, which http.NewRequest would normalise.
func send(t *testing.T, addr, method, target string) *http.Response {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: proxy\r\nConnection: close\r\n\r\n", method, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

var forwardedTargets = []struct {
	name, method, target string
}{
	{"plain route", "GET", "/api/v10/guilds/103039963636301824/channels?with_counts=true"},
	{"unicode emoji", "PUT", reactionsPath + "%F0%9F%98%8B/@me"},
	{"custom emoji", "PUT", reactionsPath + "PandaOhShit%3A863985751205085195/@me"},
	{"keycap number sign emoji", "PUT", reactionsPath + "%23%EF%B8%8F%E2%83%A3/@me"},
	{"encoded number sign", "GET", emojisPath + "a%23b?limit=5"},
	{"encoded question mark", "GET", emojisPath + "a%3Fb?limit=5"},
	{"encoded slash", "DELETE", emojisPath + "a%2Fb"},
	{"encoded percent", "GET", emojisPath + "a%2523b"},
}

func TestRequestTargetReachesDiscordAsReceived(t *testing.T) {
	for _, tt := range forwardedTargets {
		t.Run(tt.name, func(t *testing.T) {
			discord := &recorder{}
			resp := send(t, newProxy(t, discord), tt.method, tt.target)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			want := []string{requestLine(tt.method, tt.target)}
			if got := discord.requests(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("discord received %q, want %q", got, want)
			}
		})
	}
}

func TestRequestTargetReachesClusterNodeAsReceived(t *testing.T) {
	ConfigureDiscordHTTPClient("", 5*time.Second, "", false, true, false)
	manager := NewQueueManager(50, 10)

	for _, tt := range forwardedTargets {
		t.Run(tt.name, func(t *testing.T) {
			node := &recorder{}
			server := httptest.NewServer(node)
			defer server.Close()

			resp, err := manager.routeRequest(server.Listener.Addr().String(), httptest.NewRequest(tt.method, tt.target, nil))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			want := []string{requestLine(tt.method, tt.target)}
			if got := node.requests(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("node received %q, want %q", got, want)
			}
		})
	}
}
