// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

/**
 * Small helpers for mounting Lit components in jsdom.
 *
 * Every AI_LM page renders into the light DOM (`createRenderRoot() { return
 * this }`) so Tailwind utilities apply, which means assertions can read
 * `element.textContent` directly — no shadow-root traversal.
 */
import type { LitElement } from 'lit';

/** Lets pending microtasks and a macrotask turn settle (in-flight fetches). */
export function flush(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

/**
 * Mounts a registered custom element and waits for its first render plus any
 * data load kicked off in connectedCallback.
 */
export async function mount<T extends LitElement>(tag: string): Promise<T> {
  const el = document.createElement(tag) as T;
  document.body.append(el);
  await el.updateComplete;
  await flush();
  await el.updateComplete;
  return el;
}

/** Collapses whitespace so assertions are not hostage to template indentation. */
export function text(node: Element | null | undefined): string {
  return (node?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

/** Clicks the first element whose collapsed text contains `label`. */
export async function clickByText(
  host: LitElement,
  selector: string,
  label: string,
): Promise<void> {
  const target = Array.from(host.querySelectorAll(selector)).find((el) =>
    text(el).includes(label),
  ) as HTMLElement | undefined;
  if (!target) {
    throw new Error(`no ${selector} matching "${label}" — saw: ${JSON.stringify(
      Array.from(host.querySelectorAll(selector)).map((el) => text(el)),
    )}`);
  }
  target.click();
  await host.updateComplete;
  await flush();
  await host.updateComplete;
}

/** Sets an input/select value and dispatches the event the component listens for. */
export async function setValue(
  host: LitElement,
  el: HTMLInputElement | HTMLSelectElement,
  value: string,
  eventName: 'input' | 'change' = 'change',
): Promise<void> {
  el.value = value;
  el.dispatchEvent(new Event(eventName, { bubbles: true }));
  await host.updateComplete;
  await flush();
  await host.updateComplete;
}

/** Builds a JSON `Response`, mirroring what the Go handlers return. */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}
