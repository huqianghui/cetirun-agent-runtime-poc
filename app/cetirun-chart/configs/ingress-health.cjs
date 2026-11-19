// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const fs = require("node:fs");
const http = require("node:http");
const { X509Certificate } = require("node:crypto");

// Only public certificate bytes reach the application container. The private
// key remains in the managed endpoint's separate mount.
module.exports = function checkIngress(callback, root = "/var/run/cetirun/ingress-public/", port = 15440, serverName = process.env.CETIRUN_INGRESS_SERVER_NAME) {
  let leaf;
  try {
    const parse = (file) => {
      const pem = fs.readFileSync(root + file, "utf8");
      const certificates = pem.match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----/g);
      if (!certificates?.length) throw new Error("empty certificates");
      return certificates.map((value) => new X509Certificate(value));
    };
    const chain = parse("tls.crt");
    leaf = chain[0];
    if (!serverName || leaf.checkHost(serverName, { subject: "never", wildcards: false }) !== serverName ||
      !leaf.keyUsage?.includes("1.3.6.1.5.5.7.3.1")) throw new Error("serving identity");
    const roots = parse("ca.crt");
    if (roots.some((certificate) => !certificate.ca)) throw new Error("invalid trust");
    for (const certificate of [...chain, ...roots]) {
      if (Date.now() < Date.parse(certificate.validFrom) || Date.now() >= Date.parse(certificate.validTo)) {
        throw new Error("certificate validity");
      }
    }
  } catch {
    callback(false);
    return;
  }
  const request = http.get({ hostname: "127.0.0.1", port, path: "/certs", agent: false }, (response) => {
    let data = "";
    response.on("data", (chunk) => {
      data += chunk;
      if (data.length > 65536) request.destroy();
    });
    response.on("end", () => {
      try {
        const serial = (value) => String(value).replaceAll(":", "").replace(/^0+/, "").toLowerCase();
        const active = JSON.parse(data).certificates;
        callback(response.statusCode === 200 && active.some((entry) =>
          entry.cert_chain?.some((certificate) => serial(certificate.serial_number) === serial(leaf.serialNumber))));
      } catch {
        callback(false);
      }
    });
  });
  request.setTimeout(1000, () => request.destroy());
  request.on("error", () => callback(false));
};
