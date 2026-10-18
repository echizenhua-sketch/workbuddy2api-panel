package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReviveClearsDegrade 人工解冻必须清除连败降权状态。
// 回归：Revive 曾漏清 consecutiveFails/degradeUntil，导致降权期账号点「解冻」后
// 面板仍按 degrade_until 渲染冻结态，按钮不消失（直到降权自然到期）。
func TestReviveClearsDegrade(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	for i := 0; i < defaultDegradeThreshold; i++ {
		p.NoteFailures("u1")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if !st.DegradeUntil.After(time.Now()) {
		t.Fatalf("前置条件失败：应进入连败降权期，got %+v", st)
	}

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	st, _ = p.Status("u1")
	if !st.DegradeUntil.IsZero() {
		t.Errorf("Revive 应清 degrade_until，got %v", st.DegradeUntil)
	}
	if st.ConsecutiveFails != 0 {
		t.Errorf("Revive 应清 consecutive_fails，got %d", st.ConsecutiveFails)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("Revive 后账号应回到可选状态, got %+v", got)
	}
}
