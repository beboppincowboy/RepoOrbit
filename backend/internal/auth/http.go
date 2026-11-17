package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const (
	sessionCookie = "ro_session"
	stateCookie   = "ro_oauth_state"
)

type viewerKey struct{}

// FromContext returns the request's viewer, if the request was authenticated (or is in Demo mode).
func FromContext(ctx context.Context) (Viewer, bool) {
	v, ok := ctx.Value(viewerKey{}).(Viewer)
	return v, ok
}

// Handler serves the /auth/* routes and the middleware that protects other routes.
type Handler struct {
	Service *Service
	Demo    bool
	AppURL  string // browser-facing origin, e.g. https://repoorbit.example; GitHub redirects back here
	// Installed serves GitHub's post-install redirect (the app's Setup URL). Signed-in users only.
	Installed http.Handler
}

func (h *Handler) secure() bool { return strings.HasPrefix(h.AppURL, "https://") }

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/me", h.me)
	if !h.Demo {
		mux.HandleFunc("GET /auth/github/login", h.login)
		mux.HandleFunc("GET /auth/github/callback", h.callback)
		mux.Handle("POST /auth/logout", h.Protect(http.HandlerFunc(h.logout)))
		if h.Installed != nil {
			mux.Handle("GET /auth/github/installed", h.Protect(h.Installed))
		}
	}
	return mux
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"mode": "real", "user": nil, "csrfToken": nil}
	if h.Demo {
		out["mode"], out["user"] = "demo", map[string]any{"login": DemoViewer.Login, "demo": true}
	} else if token, v, err := h.session(r); err == nil {
		out["user"], out["csrfToken"] = map[string]any{"login": v.Login, "demo": false}, h.Service.CSRFToken(token)
	} else if !errors.Is(err, ErrUnauthenticated) {
		log.Printf("auth: session lookup failed: %v", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	authURL, state, err := h.Service.BeginLogin(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/auth", MaxAge: int(stateLifetime.Seconds()), HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
}

// callback never redirects anywhere but the configured app origin, so a crafted link cannot bounce users elsewhere.
func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var cookieState string
	if c, err := r.Cookie(stateCookie); err == nil {
		cookieState = c.Value
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1, HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	token, err := h.Service.CompleteLogin(r.Context(), q.Get("state"), cookieState, q.Get("code"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(SessionLifetime.Seconds()), HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, h.AppURL+"/", http.StatusFound)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("auth: sign-in failed: %v", err) // never includes tokens or codes
	http.Redirect(w, r, h.AppURL+"/login?error=signin", http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode})
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := h.Service.Logout(r.Context(), c.Value); err != nil {
			log.Printf("auth: logout failed: %v", err)
			http.Error(w, "logout failed", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) session(r *http.Request) (string, Viewer, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", Viewer{}, ErrUnauthenticated
	}
	v, err := h.Service.Viewer(r.Context(), c.Value)
	return c.Value, v, err
}

// Protect requires a signed-in viewer (the fixed demo viewer in Demo mode) and, for state-changing
// methods, a same-origin Origin header plus the CSRF token. Failures are closed: 401 or 403.
func (h *Handler) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.Demo {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, DemoViewer)))
			return
		}
		token, v, err := h.session(r)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				log.Printf("auth: session lookup failed: %v", err)
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !h.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(h.Service.CSRFToken(token))) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, v)))
	})
}

func (h *Handler) sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	want, werr := url.Parse(h.AppURL)
	return err == nil && werr == nil && origin.Scheme != "" && origin.Scheme == want.Scheme && origin.Host == want.Host
}
