// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import "@testing-library/jest-dom/vitest";

// Polyfill ResizeObserver for FluentUI components in jsdom
global.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
};

// jsdom doesn't implement scrollIntoView, which the chat view calls on new messages.
Element.prototype.scrollIntoView = () => {};
