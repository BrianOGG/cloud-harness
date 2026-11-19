import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { BrowsingContext, Builder, By, until } from 'selenium-webdriver';
import firefox from 'selenium-webdriver/firefox.js';

const [baseURL, token, outputDir, mode, approvalSession, approvalRun, approvalMessage, pendingSession, pendingRun, pendingMessage, askSession, askRun, askMessage, failedRun] = process.argv.slice(2);
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
      const grids = [...document.querySelectorAll('.run-page, .budget-grid, .conversation-layout, .stat-grid, .observability-strip, .observation-columns')];
      return {
        width: window.innerWidth,
        viewport,
        matches: matchMedia('(max-width: 768px)').matches,
        scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
        sidebar: [...document.querySelectorAll('.run-sidebar')].some(visible),
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
    if (mobileDir && width === 390 && theme === 'dark' && ['sessions', 'run', 'approvals', 'observability'].includes(screen)) {
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

async function populateMessages(entries) {
  await driver.executeScript('localStorage.setItem("cloud-harness.web-messages", JSON.stringify(JSON.parse(arguments[0])));', JSON.stringify(entries));
}

async function drainHTTP() {
  return driver.executeScript('const rows=window.__m11HttpResults||[]; window.__m11HttpResults=[]; return rows;');
}

async function instrumentHTTP() {
  await driver.executeScript(`window.__m11HttpResults=[]; if (!window.__m11OriginalFetch) window.__m11OriginalFetch=window.fetch.bind(window); window.fetch=window.__m11OriginalFetch; const original=window.__m11OriginalFetch; window.fetch=(url,init)=>{const start=performance.now(); return original(url,init).then(response=>{window.__m11HttpResults.push({url:String(url),status:response.status,duration_ms:Math.round(performance.now()-start)}); return response;});};`);
}

async function closeFlash() {
  const controls = await driver.findElements(By.css('.flash button'));
  if (controls.length) await controls[0].click();
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
    await driver.findElements(By.css('.nav-link')).then((buttons) => buttons[1].click());
    await waitFor('.large-empty');
    const observationNav = await driver.findElements(By.css('.nav-link'));
    await observationNav[2].click();
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
    const errorObservationNav = await driver.findElements(By.css('.nav-link'));
    await errorObservationNav[2].click();
    await waitFor('.observability-error');
    await waitFor('.error-message');
    assert.match(await driver.findElement(By.css('.observability-error')).getText(), /统计数据加载失败/);
    assert.match(await driver.findElement(By.css('.error-message')).getText(), /检查结果加载失败/);
    console.log('observability API failure state: explicit stats and doctor errors');
    return;
  }

  await populateMessages({ [approvalRun]: approvalMessage, [pendingRun]: pendingMessage, [askRun]: askMessage });
  await driver.navigate().refresh();
  await waitFor('.session-card');
  await captureScreen(view, 'sessions');

  const findSession = async (id) => {
    for (const button of await driver.findElements(By.css('.session-card'))) {
      if ((await button.getText()).includes(id.slice(0, 12))) return button;
    }
    throw new Error(`Session ${id} was not listed`);
  };
  const findRun = async (marker) => {
    for (const button of await driver.findElements(By.css('.run-row'))) {
      if ((await button.getText()).includes(marker)) return button;
    }
    throw new Error(`Run with marker ${marker} was not listed`);
  };

  await (await findSession(approvalSession)).click();
  await waitFor('.run-row');
  await assertMobileLayout(view, 'session', 'dark');
  await (await findRun('M11_APPROVAL_CHILD')).click();
  await waitFor('.run-summary-card');
  await driver.wait(async () => (await driver.findElement(By.css('.run-page')).getText()).includes('等待审批'), 8000);
  await setTheme('light');

  const navButtons = await driver.findElements(By.css('.nav-link'));
  await navButtons[1].click();
  await waitFor('.approval-card');
  await captureScreen(view, 'approvals');

  let mainApprovalCard;
  for (const card of await driver.findElements(By.css('.approval-card'))) {
    if ((await card.getText()).includes(approvalSession.slice(0, 12))) mainApprovalCard = card;
  }
  if (!mainApprovalCard) throw new Error('The main run is missing from the approval queue');
  await instrumentHTTP();
  await mainApprovalCard.findElement(By.css('.approval-actions .button.primary')).click();
  await waitFor('.success-flash');
  const approveRequests = await drainHTTP();
  if (!approveRequests.some((row) => row.url.includes(`/v1/runs/${approvalRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`Queue approve API failed or exceeded one second: ${JSON.stringify(approveRequests)}`);
  console.log(`UI queue Approve HTTP ${JSON.stringify(approveRequests)}`);

  await driver.findElement(By.css('.brand')).click();
  await (await findSession(approvalSession)).click();
  await waitFor('.run-row');
  await (await findRun('M11_APPROVAL_CHILD')).click();
  await waitFor('.answer-card');
  await waitFor('.timeline-title strong');
  await waitFor('.child-card');
  const runTrace = await serveJSON(`/v1/runs/${approvalRun}/trace`);
  const renderedSteps = await driver.executeScript(() => [...document.querySelectorAll('.timeline-title')].map((node) => ({
    title: node.querySelector('strong').textContent,
    actions: [...node.querySelectorAll('.action-chip')].map((chip) => chip.textContent),
  })));
  assert.equal(renderedSteps.length, runTrace.run.steps.length);
  let emptyActions = 0;
  runTrace.run.steps.forEach((step, index) => {
    const action = step.action?.trim();
    assert.deepEqual(renderedSteps[index].actions, action ? [action] : []);
    if (!action) {
      assert.equal(renderedSteps[index].title, `步骤 ${index + 1}`);
      emptyActions++;
    }
  });
  assert.ok(emptyActions, 'the real serve trace must exercise an empty action');
  console.log(`empty action trace regression: ${emptyActions} real serve steps show fallback titles without action chips`);
  await closeFlash();
  await captureScreen(view, 'run');

  await driver.findElements(By.css('.nav-link')).then((buttons) => buttons[1].click());
  await waitFor('.approval-card');
  await driver.findElement(By.css('.brand')).click();
  await (await findSession(pendingSession)).click();
  await waitFor('.run-row');
  await (await findRun('M11_PENDING_APPROVAL')).click();
  await waitFor('.run-summary-card');
  await driver.wait(async () => (await driver.findElement(By.css('.run-page')).getText()).includes('等待审批'), 8000);
  const pendingNavButtons = await driver.findElements(By.css('.nav-link'));
  await pendingNavButtons[1].click();
  const queueCards = await driver.findElements(By.css('.approval-card'));
  let pendingCard;
  for (const card of queueCards) {
    if ((await card.getText()).includes(pendingSession.slice(0, 12))) pendingCard = card;
  }
  if (!pendingCard) throw new Error('The pending approval queue entry is unavailable');
  const oldFlashClose = await driver.findElements(By.css('.flash button'));
  if (oldFlashClose.length) await oldFlashClose[0].click();
  await instrumentHTTP();
  await pendingCard.findElement(By.css('.approval-actions .button.secondary')).click();
  await driver.wait(async () => await driver.findElements(By.css('.success-flash, .error-flash')).then((elements) => elements.length > 0), 12000);
  const denyRequests = await drainHTTP();
  const denyFlash = await driver.findElement(By.css('.flash')).getText();
  console.log(`UI queue Deny message ${denyFlash}`);
  if (!(await driver.findElements(By.css('.success-flash'))).length) throw new Error(`Queue deny operation did not succeed: ${denyFlash} ${JSON.stringify(denyRequests)}`);
  if (!denyRequests.some((row) => row.url.includes(`/v1/runs/${pendingRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`Queue deny API failed or exceeded one second: ${JSON.stringify(denyRequests)}`);
  console.log(`UI queue Deny HTTP ${JSON.stringify(denyRequests)}`);
  await driver.wait(async () => {
    for (const card of await driver.findElements(By.css('.approval-card'))) {
      if ((await card.getText()).includes(pendingSession.slice(0, 12))) return false;
    }
    return true;
  }, 6000);

  await driver.findElement(By.css('.brand')).click();
  await (await findSession(askSession)).click();
  await waitFor('.run-row');
  await (await findRun('M11_ASK_USER')).click();
  await waitFor('.answer-row input');
  await assertMobileLayout(view, 'run-waiting-user', 'dark');
  await instrumentHTTP();
  const answerInput = await driver.findElement(By.css('.answer-row input'));
  await answerInput.sendKeys('staging-ui');
  await driver.findElement(By.css('.answer-row .button.primary')).click();
  await waitFor('.answer-card');
  if (!(await driver.findElement(By.css('.answer-card')).getText()).includes('staging-ui')) throw new Error('Final answer is not visible after the ask_user response');
  const answerRequests = await drainHTTP();
  if (!answerRequests.some((row) => row.url.includes(`/v1/runs/${askRun}/responses`) && row.status === 200 && row.duration_ms < 1000)) throw new Error(`ask_user response API failed or exceeded one second: ${JSON.stringify(answerRequests)}`);
  console.log(`UI ask_user answer HTTP ${JSON.stringify(answerRequests)}`);

  await closeFlash();
  const browserEmptyNav = await driver.findElements(By.css('.nav-link'));
  await browserEmptyNav[1].click();
  await waitFor('.large-empty');

  await driver.findElement(By.css('.brand')).click();
  await driver.findElement(By.css('.page-heading .button.primary')).click();
  await waitFor('.composer-card textarea');
  await instrumentHTTP();
  await driver.findElement(By.css('.composer-card textarea')).sendKeys('M11_BROWSER_SMOKE Confirm the real UI can send a live request.');
  await driver.findElement(By.css('.composer-footer .button.primary')).click();
  await waitFor('.answer-card');
  const browserSendRequests = await drainHTTP();
  if (!browserSendRequests.some((row) => row.url.includes('/v1/sessions') && row.status === 201) || !browserSendRequests.some((row) => row.url.includes('/runs') && row.status === 201)) {
    throw new Error(`Browser send flow failed: ${JSON.stringify(browserSendRequests)}`);
  }
  console.log(`UI create session and send HTTP ${JSON.stringify(browserSendRequests)}`);

  const observationNav = await driver.findElements(By.css('.nav-link'));
  await observationNav[2].click();
  await checkObservation();
  await captureScreen(view, 'observability');
  const failedDoctorRow = await waitFor('.doctor-list .doctor-fail:has(.link-button)');
  assert.match(await failedDoctorRow.getText(), /失败运行/);
  const assertFailedRun = async (source) => {
    assert.equal(await (await waitFor('.run-title-row .run-id')).getText(), failedRun);
    assert.match(await (await waitFor('.error-card')).getText(), /scripted planned failure/);
    console.log(`observability ${source} link: opened real failed run ${failedRun}`);
  };
  await failedDoctorRow.findElement(By.css('.link-button')).click();
  await assertFailedRun('doctor');
  await assertMobileLayout(view, 'run-failed', 'dark');
  await observationNav[2].click();
  await waitFor('.failure-copy code');
  const failedErrorRow = await driver.findElement(By.xpath(`//li[contains(@class,'failure-row')][.//code[text()='${failedRun}']]`));
  await failedErrorRow.findElement(By.css('.link-button')).click();
  await assertFailedRun('recent errors');
} finally {
  await driver.quit();
}
}

await main();
