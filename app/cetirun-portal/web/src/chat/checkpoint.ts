// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { byteLength, LIMITS } from "./types";

export class ResponseCheckpoint {
  text = "";
  private bytes = 0;
  private savedBytes = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private writing: Promise<void> | undefined;
  private closed = false;
  private failure: Error | undefined;

  constructor(
    private readonly write: (text: string) => Promise<unknown>,
    private readonly onError: () => void,
  ) {}

  async append(delta: string) {
    if (this.failure) throw this.failure;
    if (this.closed) throw new Error("The response is no longer being saved.");
    this.text += delta;
    this.bytes += byteLength(delta);
    if (this.bytes > LIMITS.outputBytes)
      throw new Error("The response exceeded its size limit.");
    if (this.bytes - this.savedBytes >= 16 * 1024) {
      await this.flush();
    } else if (!this.timer && !this.writing) {
      this.timer = setTimeout(() => {
        this.timer = undefined;
        void this.flush().catch(() => {});
      }, 500);
    }
  }

  private flush(): Promise<void> {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    if (this.writing) return this.writing;
    this.writing = (async () => {
      while (this.savedBytes < this.bytes) {
        const text = this.text;
        const bytes = this.bytes;
        await this.write(text);
        this.savedBytes = bytes;
      }
    })()
      .catch((reason: unknown) => {
        this.failure =
          reason instanceof Error
            ? reason
            : new Error("Chat history could not be saved.");
        this.onError();
        throw this.failure;
      })
      .finally(() => {
        this.writing = undefined;
      });
    return this.writing;
  }

  // The caller writes the final text and result together after pending saves.
  async stop() {
    this.closed = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    await this.writing;
    if (this.failure) throw this.failure;
  }
}
