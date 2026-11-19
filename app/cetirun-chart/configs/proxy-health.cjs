// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

"use strict";

const http = require("node:http");

module.exports = function checkProxy(callback, port = 15440) {
  let finished = false;
  const finish = (ready) => {
    if (finished) return;
    finished = true;
    callback(ready);
  };
  const request = http.get({ hostname: "127.0.0.1", port, path: "/ready", agent: false }, (response) => {
    response.resume();
    response.on("end", () => finish(response.statusCode === 200));
  });
  request.setTimeout(1000, () => request.destroy());
  request.on("error", () => finish(false));
};
