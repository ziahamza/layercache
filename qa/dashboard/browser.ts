// Run with DASHBOARD_LINK_FILE and DASHBOARD_BROWSER (defaults to chromium).
// A running dashboard with at least one project is required. No runtime credentials
// are read or emitted; synthetic response cases exercise the browser projection.
import { chromium, firefox, webkit } from '@playwright/test';
import fs from 'node:fs';
import assert from 'node:assert/strict';
import path from 'node:path';
(async () => {
  const link = fs.readFileSync(process.env.DASHBOARD_LINK_FILE!, 'utf8').trim();
  const output = process.env.QA_OUTPUT || fs.mkdtempSync('/tmp/layercache-browser-');
  fs.mkdirSync(output, {recursive:true});
  const browserName = process.env.DASHBOARD_BROWSER || 'chromium';
  if (browserName !== 'chromium' && browserName !== 'firefox' && browserName !== 'webkit') throw new Error('DASHBOARD_BROWSER must be chromium, firefox, or webkit');
  const browser = await {chromium, firefox, webkit}[browserName].launch({headless:true, ...(browserName === 'chromium' ? {executablePath:process.env.CHROMIUM_PATH || undefined,args:['--no-sandbox']} : {})});
  const context = await browser.newContext({viewport:{width:1440,height:1050}});
  const page = await context.newPage();
  const errors: string[] = []; const results: {name: string; passed: boolean; error?: string}[] = []; const requests: string[] = [];
  page.on('request', r => requests.push(r.url()));
  page.on('pageerror', e => errors.push(e.message));
  async function check(name: string, fn: () => Promise<void>) { try { await fn(); results.push({name,passed:true}); } catch (e) { results.push({name,passed:false,error:e instanceof Error ? e.message : String(e)}); await page.screenshot({path:path.join(output,name.replace(/\W/g,'-')+'.png'),fullPage:true}); } }
  async function settled() { await page.waitForFunction(() => !document.querySelector<HTMLButtonElement>('#refresh')!.disabled); }
  async function refresh() { await page.locator('#refresh').click(); await settled(); }
  await page.goto(link); await page.locator('.project').first().waitFor(); await settled();
  const snapshot = await page.evaluate(async (token: string) => (await fetch('/api/snapshot', {headers:{Authorization:`Bearer ${token}`}})).json(), link.split('#')[1]!);
  await check('live projects render', async () => assert.equal(await page.locator('.project').count(),snapshot.projects.length));
  await check('live storage matches projection',async()=>{for(let i=0;i<snapshot.projects.length;i++){for(const [j,key] of ['local','team'].entries()){const status=snapshot.projects[i][key].status;if(!status)continue;const card=page.locator('.project').nth(i).locator('.cache').nth(j);assert.equal(await card.locator('.metrics .value').first().innerText(),status.artifacts.toLocaleString());const meter=card.locator('progress');if(status.maxBytes>0){assert.equal(await meter.evaluate(e=>e instanceof HTMLProgressElement ? e.max : null),status.maxBytes);assert.equal(await meter.evaluate(e=>e instanceof HTMLProgressElement ? e.value : null),Math.min(status.usageBytes,status.maxBytes));}}}});
  await check('fragment and persistence', async () => assert.deepEqual(await page.evaluate(() => [location.hash,localStorage.length,sessionStorage.length,document.cookie]),['',0,0,'']));
  await page.screenshot({path:path.join(output,'desktop.png'),fullPage:true});
  await check('small text contrast', async () => {
    const failures = await page.evaluate(() => {
      function rgb(s: string) { return (s.match(/[\d.]+/g)||[]).slice(0,3).map(Number); }
      function lum(c: number[]) { return c.map(v=>{v/=255;return v<=.04045?v/12.92:((v+.055)/1.055)**2.4;}).reduce((v,n,i)=>v+n*[.2126,.7152,.0722][i],0); }
      return ['.label','.detail','.project-head p','.subtitle','.integration','.state.connected'].map(selector=>{
        const e=document.querySelector(selector); if(!e)return null;
        let ancestor: Element | null=e, bg='';
        while(ancestor){bg=getComputedStyle(ancestor).backgroundColor;if(bg!=='rgba(0, 0, 0, 0)'&&bg!=='transparent')break;ancestor=ancestor.parentElement;}
        const fgLum=lum(rgb(getComputedStyle(e).color)), bgLum=lum(rgb(bg||'rgb(255,255,255)'));
        const ratio=(Math.max(fgLum,bgLum)+.05)/(Math.min(fgLum,bgLum)+.05);
        return ratio<4.5?{selector,ratio:Number(ratio.toFixed(2))}:null;
      }).filter(Boolean);
    });
    assert.deepEqual(failures,[], 'Small text must meet 4.5:1 contrast: '+JSON.stringify(failures));
  });
  await check('dialog keyboard and accessible name', async () => {
    await page.locator('#connect').focus(); await page.keyboard.press('Enter');
    assert.equal(await page.locator('dialog[open]').count(),1);
    const accessible = await page.locator('dialog').ariaSnapshot();
    assert.match(accessible, /dialog "[^"\n]+"/,'Dialog must have accessible name');
  });
  await page.keyboard.press('Escape');
  await check('dialog focus restored', async () => assert.equal(await page.evaluate(() => document.activeElement?.id),'connect'));
  await check('dialog close button', async () => { await page.locator('#connect').click(); await page.getByRole('button',{name:'Close',exact:true}).click(); assert.equal(await page.locator('dialog[open]').count(),0); });
  await check('period request and busy state', async () => {
    const request = page.waitForRequest(r => r.url().includes('period=7d'));
    await page.selectOption('#period','7d'); await request; await settled();
  });
  await check('same session link during refresh',async()=>{
    await page.route('**/api/snapshot?*',async r=>{await new Promise(resolve=>setTimeout(resolve,300));await r.continue();});
    await page.locator('#refresh').click();await page.goto(link);await settled();
    assert.ok(await page.locator('.project').count());assert.match(await page.locator('#notice').innerText(),/Local and Team reports/);
    assert.equal(await page.evaluate(()=>location.hash),'');await page.unroute('**/api/snapshot?*');
  });
  for (const width of [320,390,600,768,1024,1440]) await check(`viewport ${width}`, async () => {
    await page.setViewportSize({width,height:844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),false);
    await page.screenshot({path:path.join(output,`viewport-${width}.png`),fullPage:true});
  });
  await page.setViewportSize({width:390,height:844});
  await check('unauthorized refresh clears snapshot', async () => {
    await page.route('**/api/snapshot?*', r=>r.fulfill({status:401,body:'expired'})); await refresh();
    assert.equal(await page.locator('.project').count(),0); assert.match(await page.locator('#notice').innerText(),/Session expired/);
    await page.unroute('**/api/snapshot?*'); await refresh(); assert.ok(await page.locator('.project').count());
  });
  await check('network failure and recovery', async () => {
    await page.route('**/api/snapshot?*', r=>r.abort()); await refresh(); assert.equal(await page.locator('.project').count(),0);
    await page.unroute('**/api/snapshot?*'); await refresh(); assert.ok(await page.locator('.project').count());
    await page.route('**/api/snapshot?*', r=>r.fulfill({status:504,body:'timeout'})); await refresh();
    assert.equal(await page.locator('.project').count(),0); assert.match(await page.locator('#notice').innerText(),/timed out/);
    await page.unroute('**/api/snapshot?*'); await refresh(); assert.ok(await page.locator('.project').count());
  });
  let mocked: typeof snapshot;
  await page.route('**/api/snapshot?*', r=>r.fulfill({json:mocked}));
  const clone=()=>JSON.parse(JSON.stringify(snapshot));
  await check('long valid project identity layout', async () => {
    mocked=clone(); mocked.projects=mocked.projects.slice(0,1); mocked.projects[0].id='github.com/'+'a'.repeat(39)+'/'+'b'.repeat(100);
    await refresh(); assert.equal(await page.evaluate(() => document.documentElement.scrollWidth>innerWidth),false);
    const head=await page.locator('.project-head').evaluate(el=>({scroll:el.scrollWidth,width:el.clientWidth})); assert.ok(head.scroll<=head.width,JSON.stringify(head));
  });
  await check('report zero negative partial evidence', async () => {
    mocked=clone(); mocked.projects=mocked.projects.slice(0,1);
    mocked.projects[0].local.report={eligible:0,hits:0,misses:0,runs:5,degraded:true,netEstimatedBuildTimeSaved:{milliseconds:-65000,known:1,total:2,confidence:'low'},integrations:[{integration:'turbo',hits:0,eligible:0}]};
    await refresh(); const text=await page.locator('.cache').first().innerText();
    assert.match(text,/No eligible outcomes/); assert.match(text,/−1.1 min/); assert.match(text,/1\/2 known · low confidence/); assert.match(text,/Partial or degraded evidence/);
  });
  await check('project text is not interpreted as HTML',async()=>{ mocked=clone(); mocked.projects[0].id='<img src=x onerror=alert(1)>';await refresh();assert.equal(await page.locator('.project img').count(),0); });
  await check('report outage preserves storage', async () => { mocked=clone(); delete mocked.projects[0].local.report;mocked.projects[0].local.reportState='unavailable';await refresh();const text=await page.locator('.cache').first().innerText();assert.match(text,/Artifact storage/);assert.match(text,/Report unavailable/);mocked.projects[0].local.reportState='authentication-required';await refresh();assert.match(await page.locator('.cache').first().innerText(),/Sign in again to read reports/); });
  await check('maximum project count renders',async()=>{mocked=clone();mocked.projects=Array.from({length:32},(_,i)=>({...mocked.projects[0],id:'github.com/team/project-'+i}));await refresh();assert.equal(await page.locator('.project').count(),32);assert.equal(await page.locator('#count').innerText(),'32');assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);});
  await page.unroute('**/api/snapshot?*'); await refresh();
  await check('second authorized tab independent',async()=>{const other=await page.context().newPage();await other.goto(link);await other.locator('.project').first().waitFor();await other.close();});
  await check('brand navigation preserves session',async()=>{await page.locator('.brand').click();await settled();assert.ok(await page.locator('.project').count(),'Brand link logs out active dashboard session');});
  await check('reload requires original link',async()=>{await page.goto('about:blank');await page.goto(link);await page.locator('.project').first().waitFor();await page.reload();await page.getByText('Open the complete dashboard link printed by the CLI',{exact:false}).waitFor();assert.equal(await page.locator('.project').count(),0);});
  await check('same tab original link restores session',async()=>{await page.goto(link);await page.locator('.project').first().waitFor({timeout:5000});await settled();assert.ok(await page.locator('.project').count(),'Original CLI link fails to restore signed-out page');assert.equal(await page.evaluate(()=>location.hash),'');});
  await check('requests remain same origin and credential free URLs',async()=>{const base=new URL(link);for(const raw of requests){if(raw==='about:blank')continue;const u=new URL(raw);assert.equal(u.origin,base.origin);assert.equal(u.hash,'');assert.equal(u.search.includes(link.split('#')[1]!),false);}});
  await check('no browser exceptions',async()=>assert.deepEqual(errors,[]));
  fs.writeFileSync(path.join(output,'results.json'),JSON.stringify(results,null,2));
  console.log(JSON.stringify({browser:browserName,output,results},null,2));await browser.close();
  if(results.some(r=>!r.passed))process.exitCode=1;
})().catch(e=>{console.error(e);process.exit(1)});
