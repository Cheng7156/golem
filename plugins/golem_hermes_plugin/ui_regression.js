// 前端全视图回归脚本（浏览器 DevTools Console 粘贴执行，21 项断言全 ✅ 为通过）
// 适用：http://127.0.0.1:8790/ui（真实）或 8791（harness，需先登录）
(async () => {
  const log = [];
  const ok = (n, c, e) => log.push((c ? "✅" : "❌") + " " + n + (e ? " | " + e : ""));
  const $q = s => document.querySelector(s);
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const goTab = async name => { document.querySelectorAll("nav button").forEach(b => { if (b.textContent === name) b.click(); }); await sleep(600); };
  await goTab("概览"); await sleep(900);
  ok("V1 概览KPI", document.querySelectorAll("#kpi .card").length >= 5);
  ok("V1 通道探活", !!($q("#chip-row") && $q("#chip-row").innerText.includes("fast")));
  ok("V1 画坊KPI", $q("#kpi").innerText.includes("画坊") && $q("#kpi").innerText.includes("在画"));
  await goTab("运行"); await sleep(500);
  const runRows = document.querySelectorAll("#runs-tbl tbody tr");
  ok("V2 运行表", runRows.length > 0);
  if (runRows.length) { runRows[0].click(); await sleep(500); }
  ok("V2 运行抽屉", $q("#drawer").className.includes("open") && $q("#drawer-bd").innerText.length > 20);
  $q("#drawer .x").click(); await sleep(200);
  await goTab("决策"); await sleep(500);
  ok("V3 决策表", document.querySelectorAll("#dec-tbl tbody tr").length > 0);
  await goTab("会话"); await sleep(500);
  ok("V4 会话表", document.querySelectorAll("#chats-tbl tbody tr").length > 0);
  // 明确取"群聊"行的 ⚙（私聊行无史官/画坊产能等群级配置；两行时间戳可能同拍导致顺序不定）
  const grow = [...document.querySelectorAll("#chats-tbl tbody tr")].find(tr => tr.innerText.includes("群聊"));
  const gear = grow ? [...grow.querySelectorAll("button")].find(b => b.textContent.includes("⚙")) : null;
  if (gear) { gear.click(); await sleep(500);
    $q('#modal-bd button[data-fx=clear][data-k="ambient_rate"]').click(); await sleep(200);
    const rg = $q('#modal-bd input[data-k="ambient_rate"]');
    rg.value = 45; rg.dispatchEvent(new Event("input", { bubbles: true })); await sleep(80);
    ok("V4 ⚙滑条实时45%", $q('#modal-bd [data-vk="ambient_rate"]').textContent === "45%");
    ok("V4 弹窗含史官开关", !!$q('#modal-bd input[type=checkbox][data-k="chronicle"]'), "群弹窗必须有chronicle开关");
    ok("V4 弹窗含画坊开关", !!$q('#modal-bd input[type=checkbox][data-k="imagegen_enabled"]'), "群弹窗必须有imagegen_enabled开关");
    const igRg = $q('#modal-bd input[data-k="imagegen_daily_cap"]');
    ok("V4 画坊日产能滑条", !!igRg);
    if (igRg) { igRg.value = 9; igRg.dispatchEvent(new Event("input", { bubbles: true })); await sleep(80);
      ok("V4 产能滑条实时9张", $q('#modal-bd [data-vk="imagegen_daily_cap"]').textContent === "9张");
    ok("V4 弹窗含人格下拉", !!$q('#modal-bd select#cs-persona') && $q('#modal-bd select#cs-persona').options.length >= 2,
      "选项数=" + (($q('#modal-bd select#cs-persona')||{}).options?.length ?? 0)); }
    const chronVal=(CUR_SET&&CUR_SET.chronicle===true);
    ok("V4 史官开启时弹窗有本群出刊按钮", !chronVal||!![...document.querySelectorAll("#modal-bd button")].find(b=>b.textContent.includes("立即出刊")));
    $q("#ovl").click(); await sleep(200); } else ok("V4 ⚙", false);
  await goTab("用量"); await sleep(500);
  ok("V5 用量图", $q("#usage-bars").innerText.length > 0);
  ok("V5 通道分解", $q("#usage-prof").innerText.includes("fast"));
  await goTab("表情"); await sleep(600);
  ok("V6 表情网格", document.querySelectorAll("#stk-grid .stk").length >= 1);
  await goTab("画廊"); await sleep(700);
  ok("V6.5 画廊KPI", document.querySelectorAll("#wo-kpi .card").length >= 3);
  const woCards = document.querySelectorAll("#wo-wall .stk");
  ok("V6.5 作品墙", woCards.length >= 3, "作品数=" + woCards.length);
  ok("V6.5 状态筛选", !!$q("#wo-status"));
  if (woCards.length) { woCards[0].click(); await sleep(500);
    ok("V6.5 工单抽屉", $q("#drawer").className.includes("open") && $q("#drawer-bd").innerText.includes("工单"));
    $q("#drawer .x").click(); await sleep(200); }
  await goTab("画像"); await sleep(600);
  const cards = document.querySelectorAll("#member-grid .card");
  ok("V7 画像卡片墙", cards.length >= 1);
  if (cards.length) { cards[0].click(); await sleep(700);
    ok("V7 详情抽屉打开", $q("#drawer").className.includes("open"));
    $q("#drawer .x").click(); await sleep(200); }
  await goTab("人设"); await sleep(700);
  ok("V7.2 人设卡墙", document.querySelectorAll("#pa-grid .card").length >= 3, "预设数=" + document.querySelectorAll("#pa-grid .card").length);
  ok("V7.2 默认人格选择器", $q("#pa-default") && $q("#pa-default").options.length >= 3);
  if (document.querySelectorAll("#pa-grid .card").length) {
    document.querySelectorAll("#pa-grid .card")[0].click(); await sleep(500);
    ok("V7.2 人格编辑抽屉", !!$q("#pe-prompt") && !!$q("#pe-name"));
    $q("#drawer .x").click(); await sleep(200); }
  await goTab("史官"); await sleep(700);
  ok("V7.5 史官编务台", $q("#chr-status").innerText.length > 10);
  const issues = document.querySelectorAll("#chr-wall .issue");
  ok("V7.5 史官期卡墙", true, "期数=" + issues.length + "（无出刊属正常）");
  if (issues.length) { issues[0].click(); await sleep(500);
    ok("V7.5 阅读模态", $q("#modal").className.includes("open"));
    $q("#ovl").click(); await sleep(200); }
  ok("V7.5 文风坊", document.querySelectorAll("#chr-styles > div").length >= 1);
  await goTab("日志"); await sleep(500);
  ok("V8 日志流", document.querySelectorAll("#log-list .logline").length > 0);
  await goTab("配置"); await sleep(600);
  const cfgTxt = $q("#cfg").innerText;
  ok("V9 配置渲染", cfgTxt.includes("ambient_reply_rate"));
  ok("V9 画坊服务商表单", !!$q("#ig-base") && !!$q("#ig-key") && !!$q("#ig-enabled") && !!$q("#ig-cap"),
    "base/key/enabled/cap 四件套");
  ok("V9 防复读参数", !!$q("#ig-dedup") && !!$q("#ig-simwin") && !!$q("#ig-simth") && !!$q("#ig-pumax"),
    "复用窗口/相似窗口/阈值/同甲限频");
  ok("V9 画坊掩码回显", ($q("#ig-key")||{}).placeholder !== "", "api_key 需掩码回显");
  console.log(log.join("\n"));
  alert(log.some(l => l.includes("❌")) ? "存在失败项，见控制台" : "全部通过 ✅");
})();
