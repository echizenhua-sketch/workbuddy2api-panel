// handler_soft_rate_strikes_test.go 连续软限流冻结门槛（soft_rate_strikes）的
// HTTP 层端到端行为与 /status 可观测性。
//
// 与 pool 包 softratestrikes_test.go 的分工：那边锁定 pool 层原语语义，
// 这里锁定「真实 handler + 真实轮转循环 + 真实错误分类」下的完整序列——
// 即用户要的「第 1、2 次 429 不冻结，第 3 次才冻结」在 HTTP 层确实成立。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSoftRateStrikesEndToEndSequence 完整序列：连续 3 次上游 429 →
// 第 1、2 次只计数（账号不冻结），第 3 次达阈才写 600s 冷却。
// 上游返回的正是生产环境观测到的 14003 "too many requests"（无重置文案）。
func TestSoftRateStrikesEndToEndSequence(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":14003,"msg":"too many requests"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

	call := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		return rec.Code
	}

	// 第 1、2 次：只计数、不冻结。这正是「不要一有 429 就冻结」的核心断言。
	for i := 1; i <= 2; i++ {
		call()
		st, ok := p.Status("u1")
		if !ok {
			t.Fatal("status(u1) missing")
		}
		if st.Cooling {
			t.Fatalf("req %d: 未达门槛（默认 3）不应冻结: %+v", i, st)
		}
		if st.SoftRateFails != i {
			t.Fatalf("req %d: soft_rate_fails=%d want %d", i, st.SoftRateFails, i)
		}
	}

	// 第 3 次达阈：冻结 600s，计数清零，退避指数推进到 1。
	if code := call(); code != 429 {
		t.Fatalf("req 3: code=%d want 429（上游原文透传 + 限流语义）", code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("req 3: 达阈应冻结: %+v", st)
	}
	if st.CoolRemaining <= 0 || st.CoolRemaining > 600 {
		t.Errorf("cool_remaining_sec=%d want (0,600]", st.CoolRemaining)
	}
	if st.SoftRateFails != 0 {
		t.Errorf("达阈后计数应清零，got %d", st.SoftRateFails)
	}
	if st.SoftStreak != 1 {
		t.Errorf("soft_streak=%d want 1（达阈才推进退避指数）", st.SoftStreak)
	}
}

// TestSoftRateStrikesConfigurableThroughHandler 门槛可配置：设为 5 时第 3 次
// 仍不冻结（证明门槛确实由配置驱动，而非写死 3）。
func TestSoftRateStrikesConfigurableThroughHandler(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":14003,"msg":"too many requests"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	p.SetSoftRateStrikes(5)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

	for i := 1; i <= 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		st, _ := p.Status("u1")
		if st.Cooling {
			t.Fatalf("req %d: 门槛=5 时不应冻结: %+v", i, st)
		}
	}
	if st, _ := p.Status("u1"); st.SoftRateFails != 3 {
		t.Errorf("soft_rate_fails=%d want 3", st.SoftRateFails)
	}
}

// TestStatusSoftRateStrikes /status 透出冻结门槛与账号级计数进度：
// 运维不必翻 state.json 即可读出「这个号还差几次就冻结」。
func TestStatusSoftRateStrikes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	// 默认门槛 3。
	read := func() map[string]any {
		h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, sseOK, true
		})})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("status not json: %v", err)
		}
		return body
	}

	body := read()
	if body["soft_rate_strikes"] != float64(3) {
		t.Errorf("soft_rate_strikes=%v want 3（默认）", body["soft_rate_strikes"])
	}

	// 攒 2 次（不冻结）→ accounts[].soft_rate_fails 应透出 2。
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	body = read()
	accts, _ := body["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("accounts=%v want 1 条", body["accounts"])
	}
	acc, _ := accts[0].(map[string]any)
	if acc["soft_rate_fails"] != float64(2) {
		t.Errorf("accounts[0].soft_rate_fails=%v want 2", acc["soft_rate_fails"])
	}
	if acc["cooling"] != false {
		t.Errorf("accounts[0].cooling=%v want false（未达门槛）", acc["cooling"])
	}

	// 门槛改为 5 → /status 立即反映（热改口径）。
	p.SetSoftRateStrikes(5)
	if got := read()["soft_rate_strikes"]; got != float64(5) {
		t.Errorf("soft_rate_strikes=%v want 5（热改后）", got)
	}
}
