package codex

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Loopback is a temporary 127.0.0.1 listener that receives the OAuth callback
// from a browser running on the server's machine.
type Loopback struct {
	RedirectURI string
	ln          net.Listener
	srv         *http.Server
	result      chan url.Values
	outcome     chan error
	closed      chan struct{}
	closeOnce   sync.Once
}

// Listen starts the callback listener. port zero tries DefaultPort and falls
// back to an ephemeral port.
func Listen(port int) (*Loopback, error) {
	if port == 0 {
		port = DefaultPort
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
	}
	lb := &Loopback{
		RedirectURI: fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, CallbackPath),
		ln:          ln,
		result:      make(chan url.Values, 1),
		outcome:     make(chan error, 1),
		closed:      make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		select {
		case lb.result <- q:
		default:
			// Another callback (or a pasted URL) already owns this attempt.
			writeCallbackPage(w, "This sign-in is already being handled. Return to the app.")
			return
		}
		// Say "Signed in" only after the server has exchanged the code and
		// stored the credentials.
		var err error
		answered := false
		select {
		case err = <-lb.outcome:
			answered = true
		case <-lb.closed:
			select {
			case err = <-lb.outcome:
				answered = true
			default:
			}
		case <-time.After(callbackWait):
		case <-r.Context().Done():
			return
		}
		switch {
		case answered && err == nil:
			writeCallbackPage(w, "Signed in. You can close this tab and return to the app.")
		case q.Get("error") != "":
			writeCallbackPage(w, "Sign-in didn't complete: "+q.Get("error"))
		case answered:
			writeCallbackPage(w, "Sign-in didn't complete. Return to the app for details and try again.")
		default:
			writeCallbackPage(w, "Sign-in is still being checked. Return to the app to see the result.")
		}
	})
	lb.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = lb.srv.Serve(ln) }()
	return lb, nil
}

// callbackWait bounds how long the callback page waits for the code exchange.
const callbackWait = 60 * time.Second

func writeCallbackPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><title>Sign in</title><body style=\"font-family:system-ui;padding:40px\">%s</body>", html.EscapeString(message))
}

// Respond reports the outcome of the callback Wait returned, so the browser
// page can show it. It never blocks, and a nil Loopback ignores it.
func (lb *Loopback) Respond(err error) {
	if lb == nil {
		return
	}
	select {
	case lb.outcome <- err:
	default:
	}
}

// Deliver hands a callback query received elsewhere (a pasted URL) to Wait. It
// reports false when one already arrived.
func (lb *Loopback) Deliver(q url.Values) bool {
	select {
	case lb.result <- q:
		return true
	default:
		return false
	}
}

// Wait returns the callback query, or an error on timeout/cancel.
func (lb *Loopback) Wait(ctx context.Context) (url.Values, error) {
	select {
	case q := <-lb.result:
		return q, nil
	case <-lb.closed:
		return nil, errors.New("sign-in was closed")
	case <-ctx.Done():
		return nil, errors.New("sign-in timed out or was cancelled")
	}
}

// Close stops the listener.
func (lb *Loopback) Close() {
	if lb == nil || lb.srv == nil {
		return
	}
	// Wake a waiting goroutine instead of leaving it parked until timeout.
	lb.closeOnce.Do(func() { close(lb.closed) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = lb.srv.Shutdown(ctx)
}
