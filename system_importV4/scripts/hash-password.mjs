#!/usr/bin/env node
// Produces the two secrets the login needs, so a plaintext password never has to be
// stored or pasted into a config file that gets copied around.
//
//   node scripts/hash-password.mjs 'the-password'
//
// The encoding must stay in step with `verifyPassword` in src/lib/auth-password.ts.

import { randomBytes, scryptSync } from "node:crypto";

const N = 16384;
const R = 8;
const P = 1;
const KEY_LEN = 32;

const plain = process.argv[2];
if (!plain) {
  console.error("ใช้: node scripts/hash-password.mjs '<password>'");
  process.exit(2);
}

const salt = randomBytes(16);
const key = scryptSync(plain, salt, KEY_LEN, { N, r: R, p: P });
// ":" not "$": dotenv-expand, systemd and compose all rewrite "$..." inside a value.
const hash = ["scrypt", N, R, P, salt.toString("base64"), key.toString("base64")].join(":");

// 48 random bytes -> 64 base64 chars, comfortably over the 32-char minimum the app
// enforces before it will accept any session at all.
const secret = randomBytes(48).toString("base64url");

console.log(`AUTH_PASSWORD_HASH=${hash}`);
console.log(`AUTH_SECRET=${secret}`);
