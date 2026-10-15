// 软限流「连续 N 次才冻结」门槛（soft_rate_strikes）的行为锁定。
//
// 背景：单次 429 常是上游瞬时频控（共享出口 IP 场景下多个账号同秒齐发），
// 一次即冻结会让健康账号被反复踢出池子——表现为面板「动不动就冻结」，
// 且 pick 返回 nil 导致客户端秒失败 503。故改为连续达阈才真正写冷却。
//
// 本文件锁定四件事：
//  1. 默认门槛 3：前 2 次只计数、不冷却；第 3 次才落冷却；
//  2. 兜底探测（已在有效软冷却中再撞）既不写冷却、**也不消耗计数**——
//     否则探测会把计数刷满并重写冷却，重新引入「越用越冷」的正反馈；
//  3. 计数清零口径：NoteSuccess / Revive / Disable；
//  4. 模型级 6004 走同一套门槛，落点换成模型级台账。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// softRateFailsOf 曝露 entry.softRateFails 供测试断言（包内私有 helper）。
func (p *Pool) softRateFailsOf(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return -1
	}
	return e.softRateFails
}

// modelCooldownCount 曝露 entry.modelCooldowns 条目数供测试断言。
func (p *Pool) modelCooldownCount(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return -1
	}
	return len(e.modelCooldowns)
}

func TestSoftRateStrikesGateDefaultThree(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 第 1、2 次：只计数，账号保持可选（面板不显示「冻结」）。
	for i := 1; i <= 2; i++ {
		if cooled := p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit"); cooled {
			t.Fatalf("call %d: 未达门槛不应写冷却", i)
		}
		if st, _ := p.Status("u1"); st.Cooling {
			t.Fatalf("call %d: 不应 cooling: %+v", i, st)
		}
		if got := p.softRateFailsOf("u1"); got != i {
			t.Fatalf("call %d: softRateFails=%d want %d", i, got, i)
		}
	}

	// 第 3 次达阈：落 600s 冷却，soft_streak 首次推进到 1。
	if cooled := p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit"); !cooled {
		t.Fatal("call 3: 达阈应写冷却")
	}
	wantCoolSec(t, p, "u1", 600, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Errorf("call 3: soft_streak=%d want 1", st.SoftStreak)
	}
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Errorf("达阈后计数应清零，got %d", got)
	}
}

func TestSoftRateStrikesSetToOneRestoresImmediateFreeze(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateStrikes(1) // 1 = 恢复旧行为

	if cooled := p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit"); !cooled {
		t.Fatal("门槛=1 时一次即应冷却")
	}
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftRateStrikesIgnoreNonPositive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateStrikes(0)  // 非法：保留默认 3
	p.SetSoftRateStrikes(-5) // 非法：保留默认 3

	for i := 1; i <= 2; i++ {
		p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Fatalf("非法门槛应回落默认 3，前 2 次不应冷却: %+v", st)
	}
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatalf("第 3 次应冷却: %+v", st)
	}
}

func TestSoftRateStrikesNotConsumedByFallbackProbe(t *testing.T) {
	// 冷却中的兜底探测：不写冷却、不延长 until、**不消耗计数**。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 3; i++ {
		p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	}
	wantCoolSec(t, p, "u1", 600, 3)

	for i := 0; i < 5; i++ {
		if cooled := p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit"); cooled {
			t.Fatalf("探测 %d: 冷却中不应重写冷却", i)
		}
	}
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Errorf("兜底探测不应消耗门槛计数，got %d want 0", got)
	}
	wantCoolSec(t, p, "u1", 600, 3) // until 未被延长
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Errorf("兜底探测不应推进 soft_streak, got %d", st.SoftStreak)
	}

	// 冷却到期后重新攒：第 3 次 → streak 2 → 600s<<1 = 1200s。
	expireCooldown(p, "u1")
	for i := 1; i <= 2; i++ {
		p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Fatalf("新一轮前 2 次不应冷却: %+v", st)
	}
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	wantCoolSec(t, p, "u1", 1200, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 2 {
		t.Errorf("soft_streak=%d want 2", st.SoftStreak)
	}
}

func TestSoftRateStrikesResetBySuccess(t *testing.T) {
	// 「连续」以成功为界：成功证明限流已解除，计数归零。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	if got := p.softRateFailsOf("u1"); got != 2 {
		t.Fatalf("softRateFails=%d want 2", got)
	}

	p.NoteSuccess("u1")
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Fatalf("NoteSuccess 应清零门槛计数，got %d", got)
	}

	// 重新攒满 3 次才冷却。
	for i := 1; i <= 2; i++ {
		p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Fatalf("成功后重新计数的前 2 次不应冷却: %+v", st)
	}
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatalf("重新攒满 3 次应冷却: %+v", st)
	}
}

func TestSoftRateStrikesResetByReviveAndDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Revive("u1") // 人工解冻 = 全清
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Fatalf("Revive 应清零门槛计数，got %d", got)
	}

	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Disable("u1", "manual") // 禁用清冷却域（含门槛进度）
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Fatalf("Disable 应清零门槛计数，got %d", got)
	}
}

func TestSoftRateStrikesModelLevelSharesGate(t *testing.T) {
	// 模型级 6004 走同一套门槛，只是落点换成模型级台账（账号级 until 不动）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)

	for i := 1; i <= 2; i++ {
		if cooled := p.NoteSoftRateForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit"); cooled {
			t.Fatalf("call %d: 未达门槛不应写模型级冷却", i)
		}
		if n := p.modelCooldownCount("u1"); n != 0 {
			t.Fatalf("call %d: modelCooldowns=%d want 0", i, n)
		}
	}
	if cooled := p.NoteSoftRateForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit"); !cooled {
		t.Fatal("call 3: 达阈应写模型级冷却")
	}
	if n := p.modelCooldownCount("u1"); n != 1 {
		t.Fatalf("达阈后 modelCooldowns=%d want 1", n)
	}
	// 账号级 until 不被模型级冷却污染（切模型即豁免）。
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("带 resetAt 的模型级冷却不应写账号级 until: %+v", st)
	}
}

func TestSoftRateFailsPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"soft_rate_fails"`) {
		t.Fatalf("state.json missing soft_rate_fails: %s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := p2.softRateFailsOf("u1"); got != 2 {
		t.Fatalf("soft_rate_fails after reload=%d want 2", got)
	}
	// 重启后继续累计：第 3 次达阈。
	if cooled := p2.NoteSoftRate("u1", 600*time.Second, time.Time{}, "x"); !cooled {
		t.Fatal("重启后第 3 次应达阈冷却")
	}
}

func TestSoftRateFailsMissingInLegacyStateFile(t *testing.T) {
	// 旧 state.json 无 soft_rate_fails → 零值兼容（门槛从零重新累计）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100,"success_count":1}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.softRateFailsOf("u1"); got != 0 {
		t.Fatalf("legacy 文件应回落零值，got %d", got)
	}
}
