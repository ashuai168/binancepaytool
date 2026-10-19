package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func securityOrder(t *testing.T, e *env, mid, base string) *Order {
	t.Helper()
	d := e.create(mid, "USDT", base, e.cbS.URL)
	o, err := e.app.st.OrderByID(d["order_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func claimHTTP(t *testing.T, e *env, o *Order, txn payTxn, cached bool) claimResult {
	t.Helper()
	if cached {
		as := e.app.matcher.stateFor(o.AccountID)
		e.app.matcher.mu.Lock()
		as.cache = []payTxn{txn}
		as.cacheAt = nowMs()
		e.app.matcher.mu.Unlock()
	} else {
		e.mock.inject(txn)
	}
	body, _ := json.Marshal(map[string]string{"binance_order_id": txn.OrderID})
	resp, err := http.Post(e.srv.URL+"/pay/"+o.Token+"/claim", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Code string      `json:"code"`
		Data claimResult `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || out.Code != "OK" {
		t.Fatalf("claim HTTP %d: %+v", resp.StatusCode, out)
	}
	return out.Data
}

func assertUnchangedPayment(t *testing.T, e *env, o *Order, txn payTxn) {
	t.Helper()
	got, err := e.app.st.OrderByID(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != o.Status || got.BinanceOrderID != "" || got.ActualAmount != 0 || got.PayerID != "" {
		t.Fatalf("rejected payment mutated order: %+v", got)
	}
	consumed, err := e.app.st.Consumed(txn.OrderID)
	if err != nil || consumed {
		t.Fatalf("rejected payment consumed: %v, %v", consumed, err)
	}
	jobs, err := e.app.st.DueCallbacks(10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("rejected payment enqueued callbacks: %v, %v", jobs, err)
	}
}

func TestClaimRejectsUnboundPayment(t *testing.T) {
	for _, mode := range []string{"add", "sub"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnvWithWorkers(t, false)
			e.app.cfg.SuffixMode = mode
			o := securityOrder(t, e, "unbound-"+mode, "0.1")
			txn := mkTxn(nextBinanceOrderID(), "0.1", "USDT", "")
			got := claimHTTP(t, e, o, txn, false)
			if got.Code != "MISMATCH" || got.Status != "pending" {
				t.Fatalf("unbound claim: %+v", got)
			}
			assertUnchangedPayment(t, e, o, txn)
		})
	}
}

func TestSmallPaymentCannotPoisonOrder(t *testing.T) {
	for _, noteBound := range []bool{false, true} {
		name := "unbound"
		if noteBound {
			name = "note-bound"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnvWithWorkers(t, false)
			o := securityOrder(t, e, "small-"+name, "100")
			note := ""
			if noteBound {
				note = o.NoteCode
			}
			small := mkTxn(nextBinanceOrderID(), "0.1", "USDT", note)
			got := claimHTTP(t, e, o, small, true)
			want := "MISMATCH"
			if noteBound {
				want = "UNDERPAID"
			}
			if got.Code != want || got.Status != "pending" {
				t.Fatalf("small claim: %+v", got)
			}
			as := e.app.matcher.stateFor(o.AccountID)
			if err := e.app.matcher.applyTxn(as, small); err != nil {
				t.Fatal(err)
			}
			assertUnchangedPayment(t, e, o, small)
			full := mkTxn(nextBinanceOrderID(), fmtAmount(o.PayAmount), "USDT", "")
			if err := e.app.matcher.applyTxn(as, full); err != nil {
				t.Fatal(err)
			}
			paid, _ := e.app.st.OrderByID(o.ID)
			if paid.Status != "paid" || paid.BinanceOrderID != full.OrderID {
				t.Fatalf("legitimate payment blocked: %+v", paid)
			}
			jobs, err := e.app.st.DueCallbacks(10)
			if err != nil || len(jobs) != 1 || jobs[0].Event != "paid" {
				t.Fatalf("expected only paid callback: %v, %v", jobs, err)
			}
		})
	}
}

func TestClaimPreservesLegitimatePayments(t *testing.T) {
	for _, mode := range []string{"add", "sub"} {
		for _, binding := range []string{"amount", "note"} {
			t.Run(mode+"/"+binding, func(t *testing.T) {
				e := newEnvWithWorkers(t, false)
				e.app.cfg.SuffixMode = mode
				o := securityOrder(t, e, "valid-"+mode+binding, "10")
				amount, note := fmtAmount(o.PayAmount), ""
				if binding == "note" {
					amount, note = "10.5", "订单 "+strings.ToLower(o.NoteCode)+" 谢谢"
				}
				txn := mkTxn(nextBinanceOrderID(), amount, "USDT", note)
				got := claimHTTP(t, e, o, txn, false)
				if got.Code != "OK" || got.Status != "paid" {
					t.Fatalf("valid claim: %+v", got)
				}
				paid, _ := e.app.st.OrderByID(o.ID)
				if paid.MatchedBy != "claim" || paid.BinanceOrderID != txn.OrderID {
					t.Fatalf("claim not persisted: %+v", paid)
				}
			})
		}
	}
}

func TestClaimCannotTakeAnotherOrdersAmount(t *testing.T) {
	for _, conflictingNote := range []bool{false, true} {
		t.Run(map[bool]string{false: "amount", true: "amount-before-note"}[conflictingNote], func(t *testing.T) {
			e := newEnvWithWorkers(t, false)
			victim := securityOrder(t, e, "victim", "10")
			other := securityOrder(t, e, "other", "10")
			note := ""
			if conflictingNote {
				note = victim.NoteCode
			}
			txn := mkTxn(nextBinanceOrderID(), fmtAmount(other.PayAmount), "USDT", note)
			got := claimHTTP(t, e, victim, txn, true)
			if got.Code != "MISMATCH" {
				t.Fatalf("cross-order claim accepted: %+v", got)
			}
			assertUnchangedPayment(t, e, victim, txn)
			got = claimHTTP(t, e, other, txn, true)
			if got.Code != "OK" || got.Status != "paid" {
				t.Fatalf("rightful claim blocked: %+v", got)
			}
		})
	}
}

func TestPaymentTimeWindow(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, binding := range []string{"amount", "note"} {
			for _, age := range []string{"missing", "before-order", "future", "after-grace"} {
				name := map[bool]string{false: "fresh", true: "cached"}[cached] + "/" + binding + "/" + age
				t.Run(name, func(t *testing.T) {
					e := newEnvWithWorkers(t, false)
					o := securityOrder(t, e, "time-window", "10")
					if age == "after-grace" {
						o.CreatedAt = nowMs() - int64(2*time.Hour/time.Millisecond)
						o.ExpiresAt = o.CreatedAt + 1000
						_, err := e.app.st.db.Exec(`UPDATE orders SET created_at=?, expires_at=? WHERE id=?`, o.CreatedAt, o.ExpiresAt, o.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
					amount, note := fmtAmount(o.PayAmount), ""
					if binding == "note" {
						amount, note = "10", o.NoteCode
					}
					txn := mkTxn(nextBinanceOrderID(), amount, "USDT", note)
					switch age {
					case "missing":
						txn.Time = 0
					case "before-order":
						txn.Time = o.CreatedAt - 1
					case "future":
						txn.Time = nowMs() + int64(3*time.Minute/time.Millisecond)
					case "after-grace":
						txn.Time = o.ExpiresAt + e.app.cfg.ExpiredGrace*1000
					}
					got := claimHTTP(t, e, o, txn, cached)
					if got.Code != "TIME_WINDOW" {
						t.Fatalf("invalid-time claim: %+v", got)
					}
					if err := e.app.matcher.applyTxn(e.app.matcher.stateFor(o.AccountID), txn); err != nil {
						t.Fatal(err)
					}
					assertUnchangedPayment(t, e, o, txn)
				})
			}
		}
	}
}

func TestClaimRecoveryDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "within-seven-days", true: "past-seven-days"}[expired], func(t *testing.T) {
			e := newEnvWithWorkers(t, false)
			o := securityOrder(t, e, "recovery", "10")
			days := int64(5)
			if expired {
				days = 8
			}
			o.CreatedAt = nowMs() - days*24*60*60*1000
			o.ExpiresAt = o.CreatedAt + 900*1000
			o.Status = "expired"
			_, err := e.app.st.db.Exec(`UPDATE orders SET created_at=?,expires_at=?,status='expired' WHERE id=?`, o.CreatedAt, o.ExpiresAt, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			txn := mkTxn(nextBinanceOrderID(), fmtAmount(o.PayAmount), "USDT", "")
			txn.Time = o.CreatedAt + 1000
			got := claimHTTP(t, e, o, txn, true)
			if expired {
				if got.Code != "STATE" {
					t.Fatalf("old order revived: %+v", got)
				}
				assertUnchangedPayment(t, e, o, txn)
			} else if got.Code != "OK" || got.Status != "paid" {
				t.Fatalf("on-time payment recovery blocked: %+v", got)
			}
		})
	}
}

func TestAmbiguousNotesDoNotChangeOrders(t *testing.T) {
	e := newEnvWithWorkers(t, false)
	a := securityOrder(t, e, "ambiguous-a", "10")
	b := securityOrder(t, e, "ambiguous-b", "10")
	txn := mkTxn(nextBinanceOrderID(), "10", "USDT", a.NoteCode+" "+b.NoteCode)
	if err := e.app.matcher.applyTxn(e.app.matcher.stateFor(a.AccountID), txn); err != nil {
		t.Fatal(err)
	}
	got := claimHTTP(t, e, a, txn, true)
	if got.Code != "MISMATCH" {
		t.Fatalf("ambiguous note accepted: %+v", got)
	}
	assertUnchangedPayment(t, e, a, txn)
	assertUnchangedPayment(t, e, b, txn)
}

func TestConcurrentClaimsFinishOnce(t *testing.T) {
	e := newEnvWithWorkers(t, false)
	o := securityOrder(t, e, "concurrent-claim", "10")
	txn := mkTxn(nextBinanceOrderID(), fmtAmount(o.PayAmount), "USDT", "")
	as := e.app.matcher.stateFor(o.AccountID)
	as.cache = []payTxn{txn}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.app.matcher.Claim(o, txn.OrderID)
			if err != nil || got.Code != "OK" || got.Status != "paid" {
				t.Errorf("concurrent claim: %+v, %v", got, err)
			}
		}()
	}
	wg.Wait()
	jobs, err := e.app.st.DueCallbacks(10)
	if err != nil || len(jobs) != 1 || jobs[0].Event != "paid" {
		t.Fatalf("duplicate callbacks: %v, %v", jobs, err)
	}
}
