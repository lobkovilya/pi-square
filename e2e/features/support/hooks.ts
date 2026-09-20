import { AfterAll, BeforeAll, setDefaultTimeout } from "@cucumber/cucumber";
import { setupSuite, teardownSuite } from "../../src/harness.js";

setDefaultTimeout(120_000);

BeforeAll({ timeout: 240_000 }, async () => {
  const started = Date.now();
  await setupSuite();
  console.log(`Live suite setup completed in ${Date.now() - started}ms`);
});

AfterAll(async () => {
  await teardownSuite();
});
