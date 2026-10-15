package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSExpirySnapshotLocalDateAndZeroValue 覆盖 renderExpirySnapshot 的三处缺陷
// （上游 PR #152 引入，2026-10-15 在 104 上用线上真实数据实测复现）。
//
// 为什么单独一个文件：上游 frontend_test.go 改动频繁，把本地补充断言塞进去会让每次合并
// 上游都在同一区域冲突。独立文件永不冲突，代价只是多一个测试入口。
//
// 三个断言（都是「与运行机时区无关」的写法：期望值在脚本内用同一套本地日期 API 现算）：
//
//  1. 零值到期时间 —— Go 侧 CreditsEarliestExpiry 是非指针 time.Time + omitempty，
//     而 omitempty 对 struct 无效，无到期信息时序列化出 "0001-01-01T00:00:00Z"（既不
//     省略也非 null）。该串 truthy 且能解析成极小时间戳，会被当成有效排序键而排到
//     **最前**，与「无到期信息排在最后」的意图相反。
//  2. 到期日必须按**本地墙钟日期**渲染。原实现用 toISOString()（UTC 日期），本地 08:00
//     前到期的批次会显示成前一天。
//  3. 剩余天数必须按**日期**算，与明细路径 expDaysLeft 同口径。原实现对瞬时值取 ceil，
//     带时分秒的到期时刻会多算一天（线上批次实测全部带时分秒），于是同一张卡片上
//     「日期」与「N 天后」自相矛盾，也与明细视图不一致。
//
// 时区：显式以 TZ=Asia/Shanghai 运行 node，让第 2 条断言具备判别力；即使宿主不认该
// 环境变量，断言退化为「等于本地日期」这一在任何时区都成立的正确性要求，不会误报。
func TestAppJSExpirySnapshotLocalDateAndZeroValue(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry snapshot test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function renderExpirySnapshot');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry snapshot region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpirySnapshot = renderExpirySnapshot;', ctx);

const p2 = n => String(n).padStart(2, '0');
const localDate = d => d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate());
// at(n, hh) = 本地「今天 + n 天」的 hh:00 对应的绝对时刻（ISO）。
// hh 取 3 会落在本地 08:00 之前 —— 正是 toISOString() 取 UTC 日期时会差一天的时刻。
const at = (n, hh) => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n); d.setHours(hh, 0, 0, 0);
  return d.toISOString();
};
const today = new Date(); today.setHours(0, 0, 0, 0);
// 与明细路径 expDaysLeft(dateStr, today) 完全同口径的期望天数。
const expDays = iso => {
  const d = new Date(iso);
  return Math.round((new Date(d.getFullYear(), d.getMonth(), d.getDate()) - today) / 86400000);
};

const soon = at(2, 3);   // 本地第 3 天凌晨 3 点：同时暴露缺陷 2 与缺陷 3
const late = at(9, 21);  // 本地第 10 天 21 点：对照组
const d = { accounts: [
  { uid: 'zzzzzzzz-0', nickname: '无到期信息',
    credits_earliest_expiry: '0001-01-01T00:00:00Z', credits_earliest_remaining: 0 },
  { uid: 'aaaaaaaa-1', nickname: '凌晨批',
    credits_earliest_expiry: soon, credits_earliest_remaining: 300 },
  { uid: 'bbbbbbbb-2', nickname: '晚间批',
    credits_earliest_expiry: late, credits_earliest_remaining: 900 },
] };
ctx.renderExpirySnapshot(d);
const html = sinks['expList'] ? sinks['expList'].innerHTML : '';
const names = [...html.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
const rows = [...html.matchAll(/最近到期 <b>([^<]+)<\/b>（([^）]+)）/g)].map(m => ({ date: m[1], dayWord: m[2] }));
process.stdout.write(JSON.stringify({
  order: names.join('|'),
  orderWant: '凌晨批|晚间批|无到期信息',
  soonDate: rows[0] ? rows[0].date : null,
  soonDateWant: localDate(new Date(soon)),
  soonDay: rows[0] ? rows[0].dayWord : null,
  soonDayWant: expDays(soon) + ' 天后到期',
  lateDate: rows[1] ? rows[1].date : null,
  lateDateWant: localDate(new Date(late)),
  lateDay: rows[1] ? rows[1].dayWord : null,
  lateDayWant: expDays(late) + ' 天后到期',
  tz: (typeof Intl !== 'undefined' && Intl.DateTimeFormat) ? Intl.DateTimeFormat().resolvedOptions().timeZone : '?',
}));`
	f, err := os.CreateTemp(t.TempDir(), "expsnapfix-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cmd := exec.Command(node, f.Name(), "app.js")
	cmd.Env = append(os.Environ(), "TZ=Asia/Shanghai")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expiry snapshot node test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))

	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("node 输出不是 JSON: %v\n%s", err, got)
	}
	check := func(key string) {
		t.Helper()
		g, w := m[key], m[key+"Want"]
		if g != w {
			t.Errorf("%s = %v, want %v（TZ=%v）", key, g, w, m["tz"])
		}
	}
	check("order")
	check("soonDate")
	check("soonDay")
	check("lateDate")
	check("lateDay")
}
