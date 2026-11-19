import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { Builder, By, until } from 'selenium-webdriver';
import firefox from 'selenium-webdriver/firefox.js';

const [baseURL, token, outputDir, mode, approvalSession, approvalRun, approvalMessage, pendingSession, pendingRun, pendingMessage, askSession, askRun, askMessage] = process.argv.slice(2);
const firefoxBinary = process.env.M11_FIREFOX_BINARY || '/tmp/m11-8-firefox/usr/lib/firefox/firefox';
const geckoBinary = process.env.M11_GECKODRIVER || '/tmp/m11-8-gecko/usr/bin/geckodriver';
await mkdir(outputDir, { recursive: true });
const options = new firefox.Options().setBinary(firefoxBinary).addArguments('-headless');
const service = new firefox.ServiceBuilder(geckoBinary);
const driver = await new Builder().forBrowser('firefox').setFirefoxOptions(options).setFirefoxService(service).build();

async function save(name) {
  await writeFile(path.join(outputDir, name), Buffer.from(await driver.takeScreenshot(), 'base64'));
  console.log(`screenshot ${path.join(outputDir, name)}`);
}

async function setTheme(value) {
  const current = await driver.executeScript('return document.documentElement.dataset.theme');
  if (current !== value) {
    await driver.findElement(By.css('.theme-toggle')).click();
    await driver.wait(async () => (await driver.executeScript('return document.documentElement.dataset.theme')) === value, 3000);
  }
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
  await driver.manage().window().setRect({ width: 1440, height: 1500, x: 0, y: 0 });
  await driver.get(`${baseURL}/ui/`);
  await driver.executeScript('localStorage.setItem("cloud-harness.http-token", arguments[0]); localStorage.setItem("cloud-harness.web-theme", "light");', token);

  if (mode === 'empty') {
    await driver.navigate().refresh();
    await waitFor('.topbar');
    await driver.findElements(By.css('.nav-link')).then((buttons) => buttons[1].click());
    await waitFor('.large-empty');
    await save('approvals-empty-light.png');
    await setTheme('dark');
    await save('approvals-empty-dark.png');
    await driver.executeScript('localStorage.removeItem("cloud-harness.http-token")');
    await driver.navigate().refresh();
    await waitFor('.error-flash');
    const authMessage = await driver.findElement(By.css('.error-flash')).getText();
    if (!authMessage.includes('cloud-harness.http-token')) throw new Error(`Missing-token message is unclear: ${authMessage}`);
    console.log(`no-token browser notice: ${authMessage}`);
    return;
  }

  await populateMessages({ [approvalRun]: approvalMessage, [pendingRun]: pendingMessage, [askRun]: askMessage });
  await driver.navigate().refresh();
  await waitFor('.session-card');
  await setTheme('light');
  await save('sessions-light.png');
  await setTheme('dark');
  await save('sessions-dark.png');
  await setTheme('light');

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
  await (await findRun('M11_APPROVAL_CHILD')).click();
  await waitFor('.run-summary-card');
  await driver.wait(async () => (await driver.findElement(By.css('.run-page')).getText()).includes('等待审批'), 8000);
  await driver.sleep(400);
  await setTheme('light');
  await save('run-waiting-light.png');
  await setTheme('dark');
  await save('run-waiting-dark.png');

  const navButtons = await driver.findElements(By.css('.nav-link'));
  await navButtons[1].click();
  await waitFor('.approval-card');
  await driver.sleep(300);
  await setTheme('light');
  await save('approvals-light.png');
  await setTheme('dark');
  await save('approvals-dark.png');

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
  await driver.sleep(250);
  await closeFlash();
  await setTheme('light');
  await save('run-light.png');
  await setTheme('dark');
  await save('run-dark.png');

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
  await setTheme('light');
  await save('approvals-empty-light.png');
  await setTheme('dark');
  await save('approvals-empty-dark.png');

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
} finally {
  await driver.quit();
}
}

await main();
