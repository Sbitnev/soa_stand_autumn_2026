package main

// Заказы с дашборда. POST /admin/orders шлёт поток POST /orders в Orders,
// POST /admin/orders/check опрашивает GET /orders/{id} по id, которые Orders
// вернул на эти заказы. Адрес Orders — только из ORDERS_URL: запрос к
// /admin/orders не может увести заглушки на произвольный адрес.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxSendOrders  = 500              // заказов за одну отправку
	maxSendRate    = 50.0             // заказов в секунду
	maxSendSeconds = 600              // длительность одной отправки, с
	orderWait      = 10 * time.Second // сколько ждать ответа на POST /orders
	statusWait     = 3 * time.Second  // сколько ждать ответа на GET /orders/{id}
	maxCheckIDs    = 500              // сколько последних id опрашивать
	maxKnownIDs    = 5000             // сколько id помнить (с reset — заново)
	maxIDLen       = 256              // id длиннее не запоминаются
)

// sending — текущая или последняя отправка заказов.
type sending struct {
	total, sent, done int
	rate              float64
	active            bool
}

type sendingView struct {
	Total    int     `json:"total"`
	Sent     int     `json:"sent"`
	Done     int     `json:"done"`
	RatePerS float64 `json:"rate_per_s"`
	Active   bool    `json:"active"`
}

func (s *state) sendingLocked() *sendingView {
	if s.send == nil {
		return nil
	}
	return &sendingView{Total: s.send.total, Sent: s.send.sent, Done: s.send.done, RatePerS: s.send.rate, Active: s.send.active}
}

// ordersTarget — адрес Orders из окружения: http(s)://хост[:порт] без
// пути; иначе пусто (отправка с дашборда выключена).
func ordersTarget(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("ORDERS_URL: ожидается http://хост:порт, получено %q", raw)
	}
	return raw, nil
}

var ordersClient = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     30 * time.Second,
}}

type sendRequest struct {
	Count    int     `json:"count"`
	RatePerS float64 `json:"rate_per_s"`
}

// handleSendOrders — POST /admin/orders {count, rate_per_s}: отправить count
// заказов в Orders с темпом rate_per_s в секунду. Одновременно — одна отправка.
func (s *state) handleSendOrders(w http.ResponseWriter, r *http.Request) {
	if s.ordersURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "no_orders", "адрес Orders заглушкам не задан (ORDERS_URL)")
		return
	}
	var req sendRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "ожидается {count, rate_per_s}")
		return
	}
	switch {
	case req.Count < 1 || req.Count > maxSendOrders:
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("count: 1…%d", maxSendOrders))
		return
	case !(req.RatePerS > 0 && req.RatePerS <= maxSendRate):
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("rate_per_s: больше 0 и не больше %g", maxSendRate))
		return
	case float64(req.Count-1)/req.RatePerS > maxSendSeconds:
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("отправка дольше %d с: увеличьте темп или уменьшите число заказов", maxSendSeconds))
		return
	}
	s.mu.Lock()
	if s.send != nil && s.send.active {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "busy", "предыдущая отправка заказов ещё идёт")
		return
	}
	s.send = &sending{total: req.Count, rate: req.RatePerS, active: true}
	cur := s.send
	view := s.sendingLocked()
	s.mu.Unlock()
	go s.sendOrders(cur, req.Count, req.RatePerS)
	writeJSON(w, http.StatusAccepted, view)
}

func (s *state) sendOrders(cur *sending, n int, rate float64) {
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(float64(i) / rate * float64(time.Second)))))
		s.mu.Lock()
		cur.sent++
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.postOrder()
			s.mu.Lock()
			cur.done++
			s.mu.Unlock()
		}()
	}
	wg.Wait()
	s.mu.Lock()
	cur.active = false
	s.mu.Unlock()
}

type orderItem struct {
	SKU        string `json:"sku"`
	Qty        int    `json:"qty"`
	PriceCents int64  `json:"price_cents"`
}

type orderBody struct {
	UserID string      `json:"user_id"`
	Items  []orderItem `json:"items"`
}

var orderPrices = []int64{1990, 9900, 49900, 129900}

// genOrder — заказ как у раннера: 1–3 разные позиции из sku-1…sku-10.
func genOrder() orderBody {
	n := 1 + rand.IntN(3)
	o := orderBody{UserID: fmt.Sprintf("u-%d", 1+rand.IntN(20))}
	used := map[int]bool{}
	for len(o.Items) < n {
		k := 1 + rand.IntN(10)
		if used[k] {
			continue
		}
		used[k] = true
		o.Items = append(o.Items, orderItem{
			SKU:        fmt.Sprintf("sku-%d", k),
			Qty:        1 + rand.IntN(3),
			PriceCents: orderPrices[rand.IntN(len(orderPrices))],
		})
	}
	return o
}

// postOrder отправляет один заказ и пишет исход в ленту (service = orders):
// outcome — код ответа, «no_response» (ответа не было за orderWait) или
// «no_connection».
func (s *state) postOrder() {
	body, _ := json.Marshal(genOrder())
	ctx, cancel := context.WithTimeout(context.Background(), orderWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.ordersURL+"/orders", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	ev := event{At: start, Service: "orders", Method: http.MethodPost, Path: "/orders"}
	var rawID string
	resp, err := ordersClient.Do(req)
	if err != nil {
		ev.Outcome = "no_connection"
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			ev.Outcome = "no_response"
		}
	} else {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		ev.Outcome = strconv.Itoa(resp.StatusCode)
		var out struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &out) == nil {
			rawID = out.ID
			ev.OrderID = sanitize(out.ID, maxIDLen)
			ev.Status = sanitize(out.Status, 20)
		}
	}
	ev.LatencyMS = time.Since(start).Milliseconds()
	s.mu.Lock()
	s.addEventLocked(ev)
	// Опрашиваются только id без правки: иначе GET ушёл бы не о том заказе.
	if id := ev.OrderID; id != "" && id == rawID && len(id) <= maxIDLen && !s.knownSet[id] && len(s.known) < maxKnownIDs {
		s.knownSet[id] = true
		s.known = append(s.known, id)
	}
	s.mu.Unlock()
}

// handleCheckOrders — POST /admin/orders/check: GET /orders/{id} по
// последним maxCheckIDs id заказов, отправленных с дашборда в этом журнале.
// Ответ: {"statuses": {order_id (как в ленте): итог}}.
func (s *state) handleCheckOrders(w http.ResponseWriter, _ *http.Request) {
	if s.ordersURL == "" {
		writeErr(w, http.StatusServiceUnavailable, "no_orders", "адрес Orders заглушкам не задан (ORDERS_URL)")
		return
	}
	s.mu.Lock()
	ids := s.known
	if len(ids) > maxCheckIDs {
		ids = ids[len(ids)-maxCheckIDs:]
	}
	ids = append([]string(nil), ids...)
	s.mu.Unlock()

	out := make(map[string]string, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			st := s.getOrderStatus(id)
			mu.Lock()
			out[cutID(id)] = st
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"statuses": out})
}

func (s *state) getOrderStatus(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), statusWait)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.ordersURL+"/orders/"+url.PathEscape(id), nil)
	resp, err := ordersClient.Do(req)
	if err != nil {
		return "нет ответа"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound {
		return "404"
	}
	if resp.StatusCode != http.StatusOK {
		return "GET " + strconv.Itoa(resp.StatusCode)
	}
	var v struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(b, &v) != nil {
		return "не JSON"
	}
	return sanitize(v.Status, 20)
}

// sanitize — чужой текст для ленты: без управляющих символов, не длиннее n.
func sanitize(v string, n int) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, v)
	if r := []rune(v); len(r) > n {
		v = string(r[:n])
	}
	return v
}

// cutID — order_id так, как он хранится в ленте.
func cutID(id string) string {
	if r := []rune(id); len(r) > maxEventOrderID {
		return string(r[:maxEventOrderID])
	}
	return id
}
