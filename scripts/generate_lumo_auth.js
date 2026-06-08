const { firefox } = require('playwright');

(async () => {
  const browser = await firefox.launch({
    headless: false // IMPORTANT: must be visible for login
  });

  const context = await browser.newContext();
  const page = await context.newPage();

  console.log('Opening Lumo…');
  await page.goto('https://lumo.proton.me', {
    waitUntil: 'domcontentloaded'
  });

  console.log('');
  console.log('👉 Log in manually in the browser window.');
  console.log('👉 Once you see Lumo chat UI, come back here.');
  console.log('👉 Press ENTER in this terminal to save session.');
  console.log('');

  // wait for user  to press Enter
  await new Promise(resolve => process.stdin.once('data', resolve));

  // save the cookies
  await context.storageState({ path: 'lumo_auth.json' });

  console.log('✅ auth.json saved successfully');

  await browser.close();
})();
