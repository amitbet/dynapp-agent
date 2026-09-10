#!/usr/bin/env node

import * as NodeCrypto from "node:crypto";
import * as NodeFs from "node:fs";

const asset = process.argv[2];
if (!asset) throw new Error("Usage: node scripts/sign-release-asset.mjs <asset>");

const raw = process.env.PAINTJS_PAYLOAD_SIGNING_KEY?.trim();
if (!raw) {
  throw new Error("PAINTJS_PAYLOAD_SIGNING_KEY is not configured; the agent updater requires a .sig asset");
}

const pem = raw.includes("BEGIN") ? raw : Buffer.from(raw, "base64").toString("utf8");
const key = NodeCrypto.createPrivateKey(pem);
const digest = NodeCrypto.createHash("sha256").update(NodeFs.readFileSync(asset)).digest("hex");
const signature = NodeCrypto.sign(null, Buffer.from(digest, "ascii"), key).toString("base64");
NodeFs.writeFileSync(`${asset}.sig`, `${signature}\n`);
const pub = NodeCrypto.createPublicKey(key);
if (!NodeCrypto.verify(null, Buffer.from(digest, "ascii"), pub, Buffer.from(signature, "base64"))) {
  throw new Error("signature self-check failed");
}
console.log(`signed ${asset} (${digest})`);
