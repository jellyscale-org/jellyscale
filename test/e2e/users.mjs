// JS-033: an administrator manages users on jellyfin-web 12.4's own pages. The selectors and the texts are what those
// pages showed against Jellyfin 12.4 (compat/captures/users-admin-12.4.md); what each step changed is then read from
// the server. The one step that differs from Jellyfin, New User without every library, is D-093's, until JS-036.
import { submitSignIn, waitFor } from "./player.mjs";
import { watch } from "./evidence.mjs";
import { recordResponses } from "./conform.mjs";

const password = "Qx7-users-e2e";

export async function userSteps({ browser, base, user, adminPassword, step }) {
  const device = 'MediaBrowser Client="users", Device="users", DeviceId="users-observer", Version="1"';
  const call = async (method, path, token, body) => {
    const authorization = token ? `${device}, Token="${token}"` : device;
    const reply = await fetch(base + path, { method, headers: { authorization, "content-type": "application/json" }, body: body && JSON.stringify(body) });
    const text = await reply.text();
    return { status: reply.status, body: /^[[{]/.test(text) ? JSON.parse(text) : text };
  };
  const signIn = async (name, pw) => (await call("POST", "/Users/AuthenticateByName", null, { Username: name, Pw: pw }));
  let adminToken;
  const everyone = async () => {
    const reply = await call("GET", "/Users", adminToken);
    if (reply.status !== 200) throw new Error(`GET /Users answered ${reply.status}`);
    return reply.body;
  };
  const named = async (name) => (await everyone()).find((listed) => listed.Name === name);
  const policy = async (name) => (await named(name))?.Policy;

  const newPage = async (name, pw) => {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    watch(context, base);
    const page = await context.newPage();
    recordResponses(page, base);
    page.setDefaultTimeout(15000);
    await page.goto(`${base}/web/`);
    await submitSignIn(page, name, pw, 30000);
    await page.waitForURL(/#\/home/, { timeout: 30000 });
    return { context, page };
  };
  let admin;
  // Away to a blank page first, so that the page is built again and asks again.
  const open = async (page, hash) => {
    await page.goto("about:blank");
    await page.goto(`${base}/web/#/${hash}`);
  };
  const shows = (page, text) => page.getByText(text).filter({ visible: true }).first().waitFor({ timeout: 15000 });
  const save = (page, button = /^Save$/) => page.getByRole("button", { name: button }).filter({ visible: true }).first().click();
  // jellyfin-web hides the checkbox itself and keeps old pages in the document: the one whose label shows is meant.
  const box = (page, selector) => page.locator("label:visible").locator(selector).first();
  const tick = async (page, selector, wanted) => {
    const checkbox = box(page, selector);
    await checkbox.waitFor({ state: "attached" });
    if ((await checkbox.isChecked()) !== wanted) await checkbox.locator("xpath=ancestor::label[1]").click();
  };
  const saveSettings = async (page) => {
    await save(page);
    await shows(page, "Settings saved.");
  };
  // The page's boxes are filled from the server: wait for the user's name before changing one.
  const profile = async (id, name) => {
    await open(admin.page, `dashboard/users/${id}/profile`);
    await waitFor("the profile page to be filled", async () => (await admin.page.locator("#txtUserName").inputValue().catch(() => "")) === name, 20000);
  };

  const target = "users-target";
  const disabled = "users-disabled";
  let targetId;

  await step("users: setup signs the administrator in", async () => {
    const reply = await signIn(user, adminPassword);
    if (reply.status !== 200) throw new Error(`sign-in answered ${reply.status}`);
    adminToken = reply.body.AccessToken;
    admin = await newPage(user, adminPassword);
  });

  await step("users: New User with all libraries ticked makes the user and opens their profile", async () => {
    const { page } = admin;
    await open(page, "dashboard/users/add");
    await page.fill("#txtUsername", target);
    await page.fill("#txtPassword", password);
    await tick(page, ".chkEnableAllFolders", true);
    await save(page);
    await page.waitForURL(/dashboard\/users\/[^/]+\/profile/, { timeout: 20000 });
    targetId = (await named(target))?.Id;
    if (!targetId) throw new Error("GET /Users does not list the new user");
    if ((await signIn(target, password)).status !== 200) throw new Error("the new user cannot sign in");
  });

  await step("users: New User without all libraries makes the user and shows the generic error", async () => {
    const { page } = admin;
    await open(page, "dashboard/users/add");
    await page.fill("#txtUsername", disabled);
    await page.fill("#txtPassword", password);
    await save(page);
    await shows(page, "There was an error processing the request. Please try again later.");
    if (!(await named(disabled))) throw new Error("the user was not made");
  });

  // D-105. jellyfin-web 12.4's New User page posts a blank password as an empty one, and then opens the profile
  // (compat/captures/users-admin-12.4.md). That user has no password: Jellyfin would sign them in with the empty one.
  await step("users: New User with a blank password makes a user with no password until one is set", async () => {
    const { page } = admin;
    await open(page, "dashboard/users/add");
    await page.fill("#txtUsername", "users-blank");
    await tick(page, ".chkEnableAllFolders", true);
    await save(page);
    await page.waitForURL(/dashboard\/users\/[^/]+\/profile/, { timeout: 20000 });
    const id = (await named("users-blank"))?.Id;
    if (!id) throw new Error("GET /Users does not list the new user");
    if ((await signIn("users-blank", "")).status !== 401) throw new Error("the empty password signs in");
    await open(page, `dashboard/users/${id}/password`);
    await page.fill("#txtNewPassword", password);
    await page.fill("#txtNewPasswordConfirm", password);
    await save(page, /^Save Password$/);
    await shows(page, "Password saved.");
    if ((await signIn("users-blank", password)).status !== 200) throw new Error("the password set does not sign in");
    const removed = await call("DELETE", `/Users/${id}`, adminToken);
    if (removed.status !== 204) throw new Error(`deleting the user answered ${removed.status}`);
  });

  await step("users: the profile page saves with nothing changed", async () => {
    await profile(targetId, target);
    const before = JSON.stringify(await policy(target));
    await saveSettings(admin.page);
    if (JSON.stringify(await policy(target)) !== before) throw new Error("saving with nothing changed changed the Policy");
  });

  await step("users: the profile page disables and enables a user", async () => {
    await profile(targetId, target);
    await tick(admin.page, ".chkDisabled", true);
    await saveSettings(admin.page);
    if ((await policy(target)).IsDisabled !== true) throw new Error("the user is not disabled");
    if ((await signIn(target, password)).status !== 401) throw new Error("the disabled user can sign in");
    await profile(targetId, target);
    await tick(admin.page, ".chkDisabled", false);
    await saveSettings(admin.page);
    if ((await signIn(target, password)).status !== 200) throw new Error("the enabled user cannot sign in");
  });

  await step("users: the profile page makes a user an administrator and demotes them", async () => {
    for (const wanted of [true, false]) {
      await profile(targetId, target);
      await tick(admin.page, ".chkIsAdmin", wanted);
      await saveSettings(admin.page);
      if ((await policy(target)).IsAdministrator !== wanted) throw new Error(`IsAdministrator is not ${wanted}`);
    }
  });

  for (const [tab, ready] of [["parentalcontrol", "#selectMaxParentalRating"], ["access", ".chkEnableAllDevices"]]) {
    await step(`users: the ${tab} page saves`, async () => {
      await open(admin.page, `dashboard/users/${targetId}/${tab}`);
      await admin.page.locator(ready).first().waitFor({ state: "attached" });
      await admin.page.waitForLoadState("networkidle");
      await saveSettings(admin.page);
    });
  }

  await step("users: an administrator's empty new password is refused, the page says so and the old one stays", async () => {
    const { page } = admin;
    await open(page, `dashboard/users/${targetId}/password`);
    await page.locator("#txtNewPassword").waitFor();
    await save(page, /^Save Password$/);
    await shows(page, "Invalid username or password. Please try again.");
    if ((await signIn(target, password)).status !== 200) throw new Error("the old password no longer signs in");
  });

  await step("users: the password page sets another user's password", async () => {
    const { page } = admin;
    await open(page, `dashboard/users/${targetId}/password`);
    await page.fill("#txtNewPassword", `${password}-two`);
    await page.fill("#txtNewPasswordConfirm", `${password}-two`);
    await save(page, /^Save Password$/);
    await shows(page, "Password saved.");
    if ((await signIn(target, `${password}-two`)).status !== 200) throw new Error("the new password does not sign in");
  });

  await step("users: a viewer changes their own password on their profile page", async () => {
    const viewer = await newPage(target, `${password}-two`);
    const { page } = viewer;
    const change = async (current, next = `${password}-three`) => {
      await open(page, `userprofile?userId=${targetId}`);
      await page.fill("#txtCurrentPassword", current);
      await page.fill("#txtNewPassword", next);
      await page.fill("#txtNewPasswordConfirm", next);
      await save(page, /^Save Password$/);
    };
    try {
      await change("wrong");
      await shows(page, "Invalid username or password. Please try again.");
      await change(`${password}-two`, "");
      await shows(page, "Invalid username or password. Please try again.");
      if ((await signIn(target, `${password}-two`)).status !== 200) throw new Error("an empty new password was taken");
      await change(`${password}-two`);
      await shows(page, "Password saved.");
      if ((await signIn(target, `${password}-three`)).status !== 200) throw new Error("the new password does not sign in");
    } finally {
      await viewer.context.close();
    }
  });

  await step("users: the user list shows every user, a disabled one included", async () => {
    const listed = await call("POST", `/Users/${(await named(disabled)).Id}/Policy`, adminToken, { ...(await policy(disabled)), IsDisabled: true });
    if (listed.status !== 204) throw new Error(`disabling answered ${listed.status}`);
    await open(admin.page, "dashboard/users");
    for (const one of await everyone()) await shows(admin.page, one.Name);
  });

  await step("users: the Quick Connect page lists every enabled user to an administrator", async () => {
    await open(admin.page, "quickconnect");
    const wanted = (await everyone()).filter((one) => !one.Policy.IsDisabled).map((one) => one.Name).sort();
    if (!wanted.includes(target) || wanted.includes(disabled)) throw new Error(`GET /Users says the enabled users are ${wanted}`);
    let options = [];
    await waitFor(`the choices ${wanted}`, async () => {
      options = (await admin.page.locator("select#userId option").allInnerTexts()).sort();
      return JSON.stringify(options) === JSON.stringify(wanted);
    }, 15000).catch(() => { throw new Error(`the page offers ${options}, want ${wanted}`); });
  });

  await step("users: delete from the user list works", async () => {
    const { page } = admin;
    for (const name of [target, disabled]) {
      await open(page, "dashboard/users");
      await page.locator(".cardBox").filter({ hasText: name }).locator(".btnUserMenu").click();
      await page.locator(".actionSheetMenuItem").filter({ hasText: "Delete" }).click();
      await page.locator(".dialogContainer button, [role=dialog] button").filter({ hasText: "Delete" }).last().click();
      await waitFor(`${name} to be gone`, async () => !(await named(name)), 15000);
    }
  });

  await admin?.context.close();
}
