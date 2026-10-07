package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "mcm_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type ctxKey struct{}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxKey{}).(*User)
	return u
}

// requireUser wraps handlers that need a logged-in user; admin-only ones also
// check the role.
func (s *Server) requireUser(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var u *User
		if c, err := r.Cookie(sessionCookie); err == nil {
			u = s.store.SessionUser(c.Value)
		}
		if u == nil {
			writeJSON(w, 401, J{"ok": false, "error": "צריך להתחבר מחדש."})
			return
		}
		if admin && !u.IsAdmin() {
			writeJSON(w, 403, J{"ok": false, "error": "רק מנהל יכול לעשות את זה."})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	}
}

func publicUser(u *User) J {
	return J{"id": u.ID, "username": u.Username, "name": u.Name, "role": u.Role, "active": u.Active}
}

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func validPassword(pw string) bool { return len([]rune(pw)) >= 4 }

func (s *Server) setSession(w http.ResponseWriter, userID int) error {
	tok, err := s.store.NewSession(userID, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

// GET /api/me: who is logged in, or whether first-time setup is needed.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if s.store.UserCount() == 0 {
		writeJSON(w, 200, J{"ok": true, "needs_setup": true})
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u := s.store.SessionUser(c.Value); u != nil {
			writeJSON(w, 200, J{"ok": true, "user": publicUser(u), "projects": s.store.Projects()})
			return
		}
	}
	writeJSON(w, 200, J{"ok": true, "user": nil})
}

type credentials struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// POST /api/setup: creates the first admin. Only works while there are no users.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in credentials
	json.NewDecoder(r.Body).Decode(&in)
	if s.store.UserCount() > 0 {
		writeJSON(w, 409, J{"ok": false, "error": "כבר קיים משתמש מנהל."})
		return
	}
	u, msg := s.createUser(in, "admin")
	if msg != "" {
		writeJSON(w, 400, J{"ok": false, "error": msg})
		return
	}
	s.setSession(w, u.ID)
	writeJSON(w, 200, J{"ok": true, "user": publicUser(&u)})
}

func (s *Server) createUser(in credentials, role string) (User, string) {
	in.Username = strings.TrimSpace(in.Username)
	in.Name = strings.TrimSpace(in.Name)
	if in.Username == "" || in.Name == "" {
		return User{}, "חסר שם או שם משתמש."
	}
	if !validPassword(in.Password) {
		return User{}, "סיסמה צריכה להיות לפחות 4 תווים."
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return User{}, err.Error()
	}
	u, err := s.store.AddUser(User{Username: in.Username, Name: in.Name, PassHash: hash, Role: role, Active: true})
	if errors.Is(err, ErrTaken) {
		return User{}, "שם המשתמש כבר תפוס."
	}
	if err != nil {
		return User{}, err.Error()
	}
	return u, ""
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	json.NewDecoder(r.Body).Decode(&in)
	u := s.store.UserByName(strings.TrimSpace(in.Username))
	if u == nil || !u.Active || bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(in.Password)) != nil {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeJSON(w, 401, J{"ok": false, "error": "שם משתמש או סיסמה שגויים."})
		return
	}
	if err := s.setSession(w, u.ID); err != nil {
		writeJSON(w, 500, J{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, J{"ok": true, "user": publicUser(u)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, J{"ok": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(in.Old)) != nil {
		writeJSON(w, 400, J{"ok": false, "error": "הסיסמה הנוכחית שגויה."})
		return
	}
	if !validPassword(in.New) {
		writeJSON(w, 400, J{"ok": false, "error": "סיסמה צריכה להיות לפחות 4 תווים."})
		return
	}
	hash, _ := hashPassword(in.New)
	s.store.UpdateUser(u.ID, func(x *User) { x.PassHash = hash })
	writeJSON(w, 200, J{"ok": true})
}

// ---------- admin: users ----------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	out := []J{}
	for _, u := range s.store.Users() {
		out = append(out, publicUser(&u))
	}
	writeJSON(w, 200, J{"users": out})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	var in credentials
	json.NewDecoder(r.Body).Decode(&in)
	role := "user"
	if in.Role == "admin" {
		role = "admin"
	}
	u, msg := s.createUser(in, role)
	if msg != "" {
		writeJSON(w, 400, J{"ok": false, "error": msg})
		return
	}
	writeJSON(w, 200, J{"ok": true, "user": publicUser(&u)})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	var in struct {
		Name     *string `json:"name"`
		Role     *string `json:"role"`
		Active   *bool   `json:"active"`
		Password *string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	var hash string
	if in.Password != nil {
		if !validPassword(*in.Password) {
			writeJSON(w, 400, J{"ok": false, "error": "סיסמה צריכה להיות לפחות 4 תווים."})
			return
		}
		hash, _ = hashPassword(*in.Password)
	}
	err := s.store.UpdateUser(id, func(u *User) {
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			u.Name = strings.TrimSpace(*in.Name)
		}
		if in.Role != nil && (*in.Role == "admin" || *in.Role == "user") {
			u.Role = *in.Role
		}
		if in.Active != nil {
			u.Active = *in.Active
		}
		if hash != "" {
			u.PassHash = hash
		}
	})
	switch {
	case errors.Is(err, ErrLastAdmin):
		writeJSON(w, 400, J{"ok": false, "error": "חייב להישאר לפחות מנהל פעיל אחד."})
	case err != nil:
		writeJSON(w, 404, J{"ok": false, "error": "משתמש לא נמצא."})
	default:
		writeJSON(w, 200, J{"ok": true})
	}
}
