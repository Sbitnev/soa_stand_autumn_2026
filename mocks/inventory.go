package main

import (
	"encoding/json"
	"net/http"
	"time"
)

type reserveRequest struct {
	OrderID string     `json:"order_id"`
	Items   []resvItem `json:"items"`
}

// logInvLocked записывает запрос к Inventory в журнал фактов и в ленту.
func (s *state) logInvLocked(at time.Time, method, op, path, orderID, outcome string) {
	s.invLogs = append(s.invLogs, invLog{At: at, Method: method, Op: op, OrderID: orderID, Outcome: outcome})
	s.addEventLocked(event{At: at, Service: "inventory", Method: method, Path: path, OrderID: orderID, Outcome: outcome})
}

func (s *state) logInv(at time.Time, method, op, path, orderID, outcome string) {
	s.mu.Lock()
	s.logInvLocked(at, method, op, path, orderID, outcome)
	s.mu.Unlock()
}

// handleReserve — POST /reservations. Ещё один вызов по order_id при действующем
// резерве отдаёт 200 с тем же резервом.
func (s *state) handleReserve(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	const path = "/reservations"
	var body reserveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.OrderID == "" || len(body.Items) == 0 {
		s.logInv(now, http.MethodPost, "reserve", path, body.OrderID, "400")
		writeErr(w, http.StatusBadRequest, "bad_request", "ожидается {order_id, items:[{sku, qty}]}")
		return
	}
	for _, it := range body.Items {
		if it.SKU == "" || it.Qty <= 0 {
			s.logInv(now, http.MethodPost, "reserve", path, body.OrderID, "400")
			writeErr(w, http.StatusBadRequest, "bad_request", "sku не пустой, qty > 0")
			return
		}
	}
	s.mu.Lock()
	s.resvRequests["reserve"]++
	if rv, ok := s.resv[body.OrderID]; ok && rv.Status == "held" {
		out := *rv
		s.logInvLocked(now, http.MethodPost, "reserve", path, body.OrderID, "200")
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	rv := &reservation{OrderID: body.OrderID, Status: "held", Items: body.Items}
	s.resv[body.OrderID] = rv
	out := *rv
	s.logInvLocked(now, http.MethodPost, "reserve", path, body.OrderID, "201")
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

// handleRelease — DELETE /reservations/{order_id}. Снимает резерв; повторный
// вызов и вызов по неизвестному order_id тоже отвечают 204.
func (s *state) handleRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("order_id")
	now := time.Now()
	s.mu.Lock()
	s.resvRequests["release"]++
	if rv, ok := s.resv[id]; ok {
		rv.Status = "released"
	}
	s.logInvLocked(now, http.MethodDelete, "release", "/reservations/{order_id}", id, "204")
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleReservations — GET /reservations?order_id=.
func (s *state) handleReservations(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("order_id")
	now := time.Now()
	const path = "/reservations"
	if id == "" {
		s.logInv(now, http.MethodGet, "get", path, "", "400")
		writeErr(w, http.StatusBadRequest, "bad_request", "нужен параметр order_id")
		return
	}
	s.mu.Lock()
	out := []reservation{}
	if rv, ok := s.resv[id]; ok {
		out = append(out, *rv)
	}
	s.logInvLocked(now, http.MethodGet, "get", path, id, "200")
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}
