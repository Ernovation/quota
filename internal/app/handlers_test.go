package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func loginForTest(t *testing.T, h http.Handler, username, password string) (*http.Cookie, int) {
	t.Helper()

	body := []byte(`{"username":"` + username + `","password":"` + password + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	res := rr.Result()
	for _, c := range res.Cookies() {
		if c.Name == "session_token" {
			return c, res.StatusCode
		}
	}
	return nil, res.StatusCode
}

func TestLoginAndMeFlow(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	cookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("expected session cookie on successful login")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/api/me status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp UserResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode /api/me response: %v", err)
	}
	if resp.Username != "admin" {
		t.Fatalf("/api/me username = %q, want %q", resp.Username, "admin")
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	cookie, status := loginForTest(t, h, "admin", "wrong-password")
	if status != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want %d", status, http.StatusUnauthorized)
	}
	if cookie != nil {
		t.Fatalf("did not expect session cookie for invalid credentials")
	}
}

func TestAuthMiddlewareRequiresSession(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me without session status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	cookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("login failed: status=%d cookieNil=%v", status, cookie == nil)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutRR := httptest.NewRecorder()
	h.ServeHTTP(logoutRR, logoutReq)
	if logoutRR.Code != http.StatusOK {
		t.Fatalf("/api/logout status = %d, want %d", logoutRR.Code, http.StatusOK)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReq.AddCookie(cookie)
	meRR := httptest.NewRecorder()
	h.ServeHTTP(meRR, meReq)
	if meRR.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after logout status = %d, want %d", meRR.Code, http.StatusUnauthorized)
	}
}

func TestAdminEndpointForbiddenForNonAdmin(t *testing.T) {
	a := newTestApp(t)
	if err := a.createUser("alice", "pw", RoleUser, 60, 30); err != nil {
		t.Fatalf("createUser error = %v", err)
	}
	h := a.Router()

	cookie, status := loginForTest(t, h, "alice", "pw")
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("user login failed: status=%d cookieNil=%v", status, cookie == nil)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("/api/admin/users for non-admin status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAdminCreateUserAndList(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	createBody := []byte(`{"username":"bob","password":"pw","role":"user","daily_quota_minutes":90,"carryover_cap_minutes":15}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/users", bytes.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.AddCookie(adminCookie)
	createRR := httptest.NewRecorder()
	h.ServeHTTP(createRR, createReq)

	if createRR.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, want %d", createRR.Code, http.StatusCreated)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	listReq.AddCookie(adminCookie)
	listRR := httptest.NewRecorder()
	h.ServeHTTP(listRR, listReq)

	if listRR.Code != http.StatusOK {
		t.Fatalf("list users status = %d, want %d", listRR.Code, http.StatusOK)
	}

	var users []UserResponse
	if err := json.Unmarshal(listRR.Body.Bytes(), &users); err != nil {
		t.Fatalf("failed to decode users response: %v", err)
	}
	found := false
	for _, u := range users {
		if u.Username == "bob" {
			found = true
			if u.DailyQuotaMinutes != 90 {
				t.Fatalf("bob quota = %d, want %d", u.DailyQuotaMinutes, 90)
			}
			if u.CarryoverCapMinutes != 15 {
				t.Fatalf("bob carryover cap = %d, want %d", u.CarryoverCapMinutes, 15)
			}
		}
	}
	if !found {
		t.Fatalf("expected bob in admin users list")
	}
}

func TestUserInternetOffEndpointWhenAlreadyOff(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	cookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("login failed: status=%d cookieNil=%v", status, cookie == nil)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/me/internet/off", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("/api/me/internet/off status = %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestLoginRejectsInvalidJSON(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("/api/login invalid json status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminUsersRequiresAuth(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("/api/admin/users without auth status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestAdminCreateUserRejectsInvalidJSON(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/users", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("/api/admin/users invalid json status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminCreateUserRejectsMissingPassword(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	body := []byte(`{"username":"eve","password":"","role":"user","daily_quota_minutes":10,"carryover_cap_minutes":5}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("/api/admin/users missing password status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminSetQuotaRejectsNegativeValue(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	body := []byte(`{"daily_quota_minutes":-1}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/admin/quota", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("set quota negative status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminSetQuotaRejectsInvalidJSON(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/admin/quota", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("set quota invalid json status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminSetCarryoverCapRejectsNegativeValue(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	body := []byte(`{"carryover_cap_minutes":-1}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/admin/carryover-cap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("set carryover cap negative status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminSetPasswordRejectsEmptyPassword(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	body := []byte(`{"password":""}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/admin/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("set password empty status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminGrantExtraMinutesRejectsInvalidJSON(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/admin/extra-minutes", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("grant extra invalid json status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAdminSetQuotaRejectsUnknownUser(t *testing.T) {
	a := newTestApp(t)
	h := a.Router()

	adminCookie, status := loginForTest(t, h, "admin", "admin")
	if status != http.StatusOK || adminCookie == nil {
		t.Fatalf("admin login failed: status=%d cookieNil=%v", status, adminCookie == nil)
	}

	body := []byte(`{"daily_quota_minutes":15}`)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/missing-user/quota", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("set quota unknown user status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}
