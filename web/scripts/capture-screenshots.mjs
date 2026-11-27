import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { BrowsingContext, Builder, By, until } from 'selenium-webdriver';
import firefox from 'selenium-webdriver/firefox.js';

const [baseURL, outputDir, mode, approvalSession, approvalRun, pendingSession, pendingRun, askSession, askRun, failedRun] = process.argv.slice(2);
const token = process.env.CLOUD_HARNESS_HTTP_TOKEN;
assert.ok(baseURL && outputDir && ['empty', 'populated'].includes(mode), 'Expected base URL, output directory, and empty/populated mode');
assert.ok(token, 'CLOUD_HARNESS_HTTP_TOKEN is required; do not pass tokens in command arguments');
const firefoxBinary = process.env.M11_FIREFOX_BINARY || '/tmp/m11-8-firefox/usr/lib/firefox/firefox';
const geckoBinary = process.env.M11_GECKODRIVER || '/tmp/m11-8-gecko/usr/bin/geckodriver';
const mobileDir = process.env.M11_MOBILE_SCREENSHOTS;
const desktop = { width: 1440, height: 1414 };
await mkdir(outputDir, { recursive: true });
const options = new firefox.Options().setBinary(firefoxBinary).addArguments('-headless').enableBidi();
const service = new firefox.ServiceBuilder(geckoBinary);
const driver = await new Builder().forBrowser('firefox').setFirefoxOptions(options).setFirefoxService(service).build();

async function save(name, dir = outputDir, width = desktop.width) {
  const image = Buffer.from(await driver.takeScreenshot(), 'base64');
  assert.equal(image.readUInt32BE(16), width, `${name} screenshot width`);
  await writeFile(path.join(dir, name), image);
  console.log(`screenshot ${path.join(dir, name)}`);
}

async function setTheme(value) {
  const current = await driver.executeScript('return document.documentElement.dataset.theme');
  if (current !== value) {
    await openSidebar();
    await driver.findElement(By.css('.theme-toggle')).click();
    await driver.wait(async () => (await driver.executeScript('return document.documentElement.dataset.theme')) === value, 3000);
  }
}

async function assertMobileLayout(view, screen, theme) {
  for (const width of [390, 768]) {
    await view.setViewport(width, 900, 1);
    const measures = await driver.executeScript(() => {
      const viewport = document.documentElement.clientWidth;
      const visible = (node) => node.getClientRects().length > 0;
      const buttons = [...document.querySelectorAll('button')].filter(visible);
      const outOfBounds = buttons.filter((button) => {
        const { left, right } = button.getBoundingClientRect();
        return left < 0 || right > viewport || button.scrollWidth > button.clientWidth + 1;
      }).map((button) => button.textContent.trim());
      const grids = [...document.querySelectorAll('.stat-grid, .observability-strip, .observation-columns')];
      return {
        width: window.innerWidth,
        viewport,
        matches: matchMedia('(max-width: 768px)').matches,
        scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
        sidebar: [...document.querySelectorAll('.sidebar')].some(visible),
        multiColumn: grids.filter((grid) => getComputedStyle(grid).gridTemplateColumns.includes(' ')).map((grid) => grid.className),
        outOfBounds,
      };
    });
    const detail = `${screen}/${theme}/${width}px: ${JSON.stringify(measures)}`;
    assert.equal(measures.width, width, detail);
    assert(measures.matches && measures.scrollWidth <= measures.viewport && !measures.sidebar, detail);
    assert.deepEqual(measures.multiColumn, [], detail);
    assert.deepEqual(measures.outOfBounds, [], detail);
    console.log(`MOBILE_CHECK ${screen} theme=${theme} width=${width} viewport=${measures.viewport} scrollWidth=${measures.scrollWidth} sidebar=collapsed columns=single buttons=contained result=PASS`);
    if (mobileDir && width === 390 && theme === 'dark' && ['welcome', 'chat', 'approvals', 'observability'].includes(screen)) {
      await mkdir(mobileDir, { recursive: true });
      await save(`${screen}-mobile-dark.png`, mobileDir, width);
    }
  }
  await view.setViewport(desktop.width, desktop.height, 1);
}

async function captureScreen(view, screen) {
  for (const theme of ['light', 'dark']) {
    await setTheme(theme);
    await save(`${screen}-${theme}.png`);
    await assertMobileLayout(view, screen, theme);
  }
}

async function serveJSON(path) {
  const response = await fetch(`${baseURL}${path}`, { headers: { Authorization: `Bearer ${token}` } });
  assert.ok(response.ok, `${path} HTTP ${response.status}`);
  return response.json();
}

async function checkObservation() {
  await waitFor('.stat-grid');
  await waitFor('.doctor-list');
  const [stats, doctor] = await Promise.all([serveJSON('/v1/stats'), serveJSON('/v1/doctor')]);
  const actual = await driver.executeScript(() => {
    const text = (selector) => [...document.querySelectorAll(selector)].map((element) => element.textContent.trim());
    return {
      metrics: text('.stat-tile strong'),
      notes: text('.stat-tile small'),
      budget: text('.strip-section:first-child .strip-values > span'),
      policy: text('.policy-count b'),
      hooks: text('.strip-section:last-child .strip-values strong'),
      errors: text('.failure-copy p'),
      overall: text('.doctor-overall .doctor-copy span'),
      doctors: text('.doctor-list .doctor-copy span'),
    };
  });
  const number = (value) => value.toLocaleString('zh-CN');
  const cost = stats.usage.cost === null ? (stats.model_calls ? '未配置价格' : '暂无用量') : new Intl.NumberFormat('zh-CN', { style: 'currency', currency: 'USD', maximumFractionDigits: 4 }).format(stats.usage.cost);
  assert.deepEqual(actual.metrics, [number(stats.sessions), number(stats.active_runs), number(stats.model_calls), cost]);
  assert.deepEqual(actual.notes, [
    `${number(stats.runs)} 次运行 · ${number(stats.events)} 个持久事件`, `${number(stats.failed_runs)} 次失败运行`,
    `${number(stats.usage.input_tokens)} 输入 · ${number(stats.usage.output_tokens)} 输出 tokens`,
    `${(stats.usage.cache_hit_rate * 100).toFixed(1)}% 缓存命中`,
  ]);
  assert.deepEqual(actual.budget, [`模型 ${number(stats.budget.model_used)} / ${number(stats.budget.model_max)}`, `工具 ${number(stats.budget.tool_used)} / ${number(stats.budget.tool_max)}`]);
  assert.deepEqual(actual.policy, [stats.policy.allow, stats.policy.ask, stats.policy.deny].map(number));
  assert.deepEqual(actual.hooks, [stats.hooks.warnings, stats.hooks.blocked].map(number));
  assert.deepEqual(actual.errors, stats.recent_errors.map((failure) => failure.message.trim()));
  assert.deepEqual(actual.overall, [doctor.find((check) => check.name === 'overall').detail]);
  assert.deepEqual(actual.doctors.sort(), doctor.filter((check) => check.name !== 'overall').map((check) => check.detail).sort());
  console.log(`observability DOM matches real serve: sessions=${stats.sessions} runs=${stats.runs} events=${stats.events} model_calls=${stats.model_calls} cost=${cost} recent_errors=${actual.errors.length}`);
}

async function waitFor(selector) {
  await driver.wait(until.elementLocated(By.css(selector)), 12000);
  return driver.findElement(By.css(selector));
}

async function drainHTTP() {
  return driver.executeScript('const rows=window.__m11HttpResults||[]; window.__m11HttpResults=[]; return rows;');
}

async function instrumentHTTP() {
  await driver.executeScript(`window.__m11HttpResults=[]; if (!window.__m11OriginalFetch) window.__m11OriginalFetch=window.fetch.bind(window); window.fetch=window.__m11OriginalFetch; const original=window.__m11OriginalFetch; window.fetch=(url,init)=>{const start=performance.now(); return original(url,init).then(response=>{window.__m11HttpResults.push({url:String(url),status:response.status,duration_ms:Math.round(performance.now()-start)}); return response;});};`);
}

const sessionItem = (id) => `.session-item[data-session-id="${id}"]`;
const approvalCard = (runId) => `.approval-card[data-run-id="${runId}"]`;
const historyMessage = (runId, kind) => `.history-message[data-run-id="${runId}"][data-kind="${kind}"]`;

async function waitForHistoryText(runId, kind, pattern) {
  const selector = historyMessage(runId, kind);
  await driver.wait(async () => {
    const rows = await driver.findElements(By.css(selector));
    for (const row of rows) if (pattern.test(await row.getText())) return true;
    return false;
  }, 12000, `Missing persisted ${kind} for run ${runId}`);
}

async function checkHistory(sessionId) {
  const messages = [];
  const cursors = new Set();
  let cursor = '';
  do {
    // Small pages exercise the complete cursor chain rather than a first-page preview.
    const query = new URLSearchParams({ limit: '2', cursor });
    const page = await serveJSON(`/v1/sessions/${sessionId}/messages?${query}`);
    assert.ok(Array.isArray(page.messages));
    assert.equal(typeof page.next_cursor, 'string');
    assert.ok(page.messages.every((message) => message.session_id === sessionId));
    messages.push(...page.messages);
    cursor = page.next_cursor;
    assert.ok(!cursor || !cursors.has(cursor), 'Repeated history cursor');
    cursors.add(cursor);
  } while (cursor);
  assert.equal(new Set(messages.map((message) => message.id)).size, messages.length, 'Duplicate server message IDs');
  const expected = messages.map((message) => ({
    id: message.id, run: message.run_id ?? null, kind: message.kind, status: message.status,
  }));
  await waitFor('.session-history');
  await driver.wait(async () => {
    const actual = await driver.executeScript(() => ({
      busy: document.querySelector('.session-history')?.getAttribute('aria-busy'),
      error: Boolean(document.querySelector('.history-error')),
      rows: [...document.querySelectorAll('.history-message')].map((node) => ({
        id: node.dataset.messageId, run: node.dataset.runId ?? null,
        kind: node.dataset.kind, status: node.dataset.status,
      })),
    }));
    return actual.busy === 'false' && !actual.error && JSON.stringify(actual.rows) === JSON.stringify(expected);
  }, 12000, `Rendered history does not match all persisted messages for ${sessionId}`);
  assert.equal(await driver.findElement(By.css('.session-item.current')).getAttribute('data-session-id'), sessionId);
  console.log(`history DOM matches serve: session=${sessionId} messages=${messages.length}`);
  return messages;
}

async function openFinishedRun(runId) {
  const selector = `.turn[data-run-id="${runId}"]`;
  await waitFor(`${selector}[data-status="COMPLETED"]`);
  const collapsed = await driver.findElements(By.css('.past-runs:not([open]) > summary'));
  if (collapsed.length) await collapsed[0].click();
  return selector;
}

// On a narrow window the sidebar is an overlay that has to be opened first.
async function openSidebar() {
  const toggles = await driver.findElements(By.css('[aria-label="展开侧栏"]'));
  if (toggles.length) await toggles[0].click();
}

async function nav(page) {
  await openSidebar();
  const index = { approvals: 2, observability: 3 }[page];
  await driver.findElement(By.css(`.sidebar-nav .nav-item:nth-child(${index})`)).click();
}

async function openSession(id) {
  await openSidebar();
  await (await waitFor(sessionItem(id))).click();
}

async function waitGone(selector) {
  await driver.wait(async () => (await driver.findElements(By.css(selector))).length === 0, 12000);
}

async function main() {
try {
  await driver.manage().window().setRect({ ...desktop, x: 0, y: 0 });
  const view = await BrowsingContext(driver, { browsingContextId: await driver.getWindowHandle() });
  await driver.get(`${baseURL}/ui/`);
  await view.setViewport(desktop.width, desktop.height, 1);
  await driver.executeScript('localStorage.setItem("cloud-harness.http-token", arguments[0]); localStorage.setItem("cloud-harness.web-theme", "light");', token);

  if (mode === 'empty') {
    await driver.navigate().refresh();
    await waitFor('.topbar');
    await waitFor('.welcome');
    await nav('approvals');
    await waitFor('.page-empty');
    await nav('observability');
    await waitFor('.observability-page');
    await waitFor('.clean-message');
    await checkObservation();
    const emptyObservation = await driver.findElement(By.css('.observability-page')).getText();
    assert.match(emptyObservation, /暂无运行错误/);
    assert.match(emptyObservation, /暂无用量/);
    console.log('empty observability state: explicit zero-value cards and doctor checks');
    await driver.executeScript('localStorage.removeItem("cloud-harness.http-token")');
    await driver.navigate().refresh();
    await waitFor('.topbar');
    await nav('observability');
    await waitFor('.observability-error');
    await waitFor('.error-message');
    assert.match(await driver.findElement(By.css('.observability-error')).getText(), /统计数据加载失败/);
    assert.match(await driver.findElement(By.css('.error-message')).getText(), /检查结果加载失败/);
    console.log('observability API failure state: explicit stats and doctor errors');
    return;
  }

  await driver.navigate().refresh();
  await waitFor('.welcome');
  await waitFor(sessionItem(approvalSession));
  await captureScreen(view, 'welcome');

  // A waiting run shows its approval card in the thread and in the queue.
  await openSession(approvalSession);
  await waitFor(`.turn[data-run-id="${approvalRun}"] .wait-card`);
  await waitForHistoryText(approvalRun, 'input', /M11_APPROVAL_CHILD/);
  await checkHistory(approvalSession);
  assert.match(await driver.findElement(By.css('.wait-card')).getText(), /write_file/);
  await assertMobileLayout(view, 'chat-waiting-approval', 'dark');
  await setTheme('light');
  await nav('approvals');
  await waitFor(approvalCard(approvalRun));
  await waitFor(approvalCard(pendingRun));
  await captureScreen(view, 'approvals');

  await instrumentHTTP();
  await driver.findElement(By.css(`${approvalCard(approvalRun)} .approval-actions .button.primary`)).click();
  await waitGone(approvalCard(approvalRun));
  const approveRequests = await drainHTTP();
  if (!approveRequests.some((row) => row.url.includes(`/v1/runs/${approvalRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`Queue approve API failed or exceeded one second: ${JSON.stringify(approveRequests)}`);
  console.log(`UI queue Approve HTTP ${JSON.stringify(approveRequests)}`);

  // The approved run finishes: answer, work block, and the run details drawer.
  await openSession(approvalSession);
  await waitForHistoryText(approvalRun, 'response', /parent task completed after approval/);
  const approvedHistory = await checkHistory(approvalSession);
  assert.ok(approvedHistory.some((message) => message.run_id === approvalRun && message.kind === 'approval_decision' && message.content === 'approve'));
  const turn = await openFinishedRun(approvalRun);
  await driver.findElement(By.css(`${turn} .work-toggle`)).click();
  await waitFor(`${turn} .work-list .work-tool`);
  const workTools = await driver.executeScript((selector) => [...document.querySelectorAll(`${selector} .work-tool`)].map((node) => node.textContent), turn);
  assert.deepEqual(workTools, ['write_file', 'list_files', 'delegate_task']);
  await driver.findElement(By.xpath(`//article[@data-run-id='${approvalRun}']//div[contains(@class,'turn-actions')]/button[@aria-label='运行详情']`)).click();
  await waitFor('.inspector .trace-call');
  const runTrace = await serveJSON(`/v1/runs/${approvalRun}/trace`);
  const rendered = await driver.executeScript(() => ({
    id: document.querySelector('.inspector-head code').textContent,
    steps: [...document.querySelectorAll('.inspector .trace > li')].map((node) => ({
      title: node.querySelector('.trace-title strong').textContent,
      calls: [...node.querySelectorAll('.trace-call code')].map((call) => call.textContent),
    })),
    budget: [...document.querySelectorAll('.inspector .budget-value')].map((node) => node.textContent.replace(/\s+/g, ' ').trim()),
  }));
  assert.equal(rendered.id, approvalRun);
  const traceSteps = runTrace.run.steps;
  assert.equal(rendered.steps.length, traceSteps.length + runTrace.run.children.length);
  traceSteps.forEach((step, index) => {
    assert.deepEqual(rendered.steps[index].calls, (step.batches || []).flatMap((batch) => (batch.calls || []).map((call) => call.name)));
    // serve leaves a step's action empty; a step without a batch has only its number to show.
    if (!step.action?.trim() && !(step.batches || []).length) assert.equal(rendered.steps[index].title, `步骤 ${index + 1}`);
  });
  assert.equal(rendered.steps[rendered.steps.length - 1].title, '子任务');
  const runView = await serveJSON(`/v1/runs/${approvalRun}`);
  assert.deepEqual(rendered.budget, [
    `${runView.budget.used_model_calls} / ${runView.budget.max_model_calls}`, `${runView.budget.used_tool_calls} / ${runView.budget.max_tool_calls}`,
    `${runView.budget.used_script_calls} / ${runView.budget.max_script_calls}`, `${runView.budget.used_child_runs} / ${runView.budget.max_child_runs}`,
  ]);
  console.log(`run details match real serve: ${traceSteps.length} steps, ${runTrace.run.children.length} child run, tools ${workTools.join(', ')}`);
  await captureScreen(view, 'chat');
  await driver.findElement(By.css('.inspector [role="tab"]:nth-child(2)')).click();
  await waitFor('.inspector .events li');
  const eventTypes = await driver.executeScript(() => [...document.querySelectorAll('.inspector .events summary strong')].map((node) => node.textContent));
  assert.ok(eventTypes.includes('run.created') && eventTypes.includes('run.completed') && eventTypes.includes('child.finished'), eventTypes.join(','));
  // Child-run facts remain in this session's transcript with their source run ID.
  for (const child of runTrace.run.children) {
    assert.ok(approvedHistory.some((message) => message.run_id === child.id), `Missing child history for ${child.id}`);
  }
  await driver.navigate().refresh();
  assert.deepEqual((await checkHistory(approvalSession)).map((message) => message.id), approvedHistory.map((message) => message.id), 'Refresh changed durable message identities');
  await openSession(askSession);
  await openSession(approvalSession);
  await checkHistory(approvalSession);

  // Deny from the queue.
  await nav('approvals');
  await waitFor(approvalCard(pendingRun));
  await instrumentHTTP();
  await driver.findElement(By.css(`${approvalCard(pendingRun)} .approval-actions .button:not(.primary)`)).click();
  await waitGone(approvalCard(pendingRun));
  const denyRequests = await drainHTTP();
  if (!denyRequests.some((row) => row.url.includes(`/v1/runs/${pendingRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`Queue deny API failed or exceeded one second: ${JSON.stringify(denyRequests)}`);
  console.log(`UI queue Deny HTTP ${JSON.stringify(denyRequests)}`);
  assert.equal((await driver.findElements(By.css('.flash'))).length, 0, 'deny left an error message');
  await openSession(pendingSession);
  await waitForHistoryText(pendingRun, 'response', /pending file request was denied and recorded/);
  const deniedHistory = await checkHistory(pendingSession);
  assert.ok(deniedHistory.some((message) => message.run_id === pendingRun && message.kind === 'approval_decision' && message.content === 'deny'));

  // ask_user: the composer answers the question.
  await openSession(askSession);
  await waitFor('.question');
  await waitForHistoryText(askRun, 'question', /Which deployment region/);
  await checkHistory(askSession);
  await assertMobileLayout(view, 'chat-waiting-user', 'dark');
  await instrumentHTTP();
  await driver.findElement(By.css('.composer textarea')).sendKeys('staging-ui');
  await driver.findElement(By.css('.composer .send')).click();
  await waitForHistoryText(askRun, 'response', /Deployment region confirmed: staging-ui/);
  await waitForHistoryText(askRun, 'answer', /staging-ui/);
  await checkHistory(askSession);
  const answerRequests = await drainHTTP();
  if (!answerRequests.some((row) => row.url.includes(`/v1/runs/${askRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`ask_user response API failed or exceeded one second: ${JSON.stringify(answerRequests)}`);
  console.log(`UI ask_user answer HTTP ${JSON.stringify(answerRequests)}`);

  await nav('approvals');
  await waitFor('.page-empty');

  // A new conversation from the welcome screen.
  await openSidebar();
  await driver.findElement(By.css('.nav-item.new-chat')).click();
  await waitFor('.welcome .composer textarea');
  await instrumentHTTP();
  await driver.findElement(By.css('.composer textarea')).sendKeys('M11_BROWSER_SMOKE Confirm the real UI can send a live request.');
  await driver.findElement(By.css('.composer .send')).click();
  await waitFor('.history-message[data-kind="response"] .history-response');
  assert.match(await driver.findElement(By.css('.session-item.current')).getText(), /M11_BROWSER_SMOKE/);
  const browserSession = await driver.findElement(By.css('.session-item.current')).getAttribute('data-session-id');
  await checkHistory(browserSession);
  const browserSendRequests = await drainHTTP();
  if (!browserSendRequests.some((row) => row.url.includes('/v1/sessions') && row.status === 201) || !browserSendRequests.some((row) => row.url.includes('/runs') && row.status === 201)) {
    throw new Error(`Browser send flow failed: ${JSON.stringify(browserSendRequests)}`);
  }
  console.log(`UI create session and send HTTP ${JSON.stringify(browserSendRequests)}`);

  await nav('observability');
  await checkObservation();
  await captureScreen(view, 'observability');
  const failedDoctorRow = await waitFor('.doctor-list .doctor-fail:has(.link-button)');
  assert.match(await failedDoctorRow.getText(), /失败运行/);
  const assertFailedRun = async (source) => {
    await waitForHistoryText(failedRun, 'run_status', /scripted planned failure/);
    assert.equal(await (await waitFor('.inspector-head code')).getText(), failedRun);
    console.log(`observability ${source} link: opened real failed run ${failedRun}`);
  };
  await failedDoctorRow.findElement(By.css('.link-button')).click();
  await assertFailedRun('doctor');
  await assertMobileLayout(view, 'chat-failed', 'dark');
  await nav('observability');
  await waitFor('.failure-copy code');
  const failedErrorRow = await driver.findElement(By.xpath(`//li[contains(@class,'failure-row')][.//code[text()='${failedRun}']]`));
  await failedErrorRow.findElement(By.css('.link-button')).click();
  await assertFailedRun('recent errors');
} finally {
  await driver.quit();
}
}

await main();
