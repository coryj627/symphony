import {test, expect} from '@playwright/test';
import config from '../../playwright.config.mjs';

test('browser server launch uses portable tool lookup', () => {
  expect(config.webServer.command).not.toMatch(/(?:\/Users\/|\/home\/|[A-Za-z]:\\)/);
  expect(config.webServer.command).toBe('npm run start:e2e');
});
