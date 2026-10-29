/**
 * Put text on the clipboard.
 *
 * Two failures are folded into one rejected promise, because they are the same thing to the
 * caller: the API is missing when the page is not a secure context (`navigator.clipboard` is
 * undefined there, so the call throws rather than rejecting), and a write can be refused by the
 * webview. Both used to be swallowed by the buttons that copy a refusal's command line, which
 * left a click that did nothing at all and a person pasting nothing into their terminal.
 */
export function copyText(text: string): Promise<void> {
  const clipboard = navigator.clipboard
  if (!clipboard) return Promise.reject(new Error('the clipboard is not available'))
  return clipboard.writeText(text)
}
