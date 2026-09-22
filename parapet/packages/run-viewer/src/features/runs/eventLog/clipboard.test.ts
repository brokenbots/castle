import { afterEach, describe, expect, test, vi } from 'vitest';
import { copyTextToClipboard } from './clipboard';

// jsdom has neither navigator.clipboard nor document.execCommand; each test
// stubs exactly what it simulates. CRI-284: the insecure-origin path must
// copy via execCommand instead of throwing on the missing clipboard API.
function stubNavigatorClipboard(clipboard: Partial<Clipboard> | undefined) {
  Object.defineProperty(navigator, 'clipboard', {
    value: clipboard,
    configurable: true,
  });
}

function stubExecCommand(impl: () => boolean) {
  Object.defineProperty(document, 'execCommand', {
    value: vi.fn(impl),
    configurable: true,
  });
}

afterEach(() => {
  delete (navigator as { clipboard?: unknown }).clipboard;
  delete (document as { execCommand?: unknown }).execCommand;
  vi.restoreAllMocks();
});

describe('copyTextToClipboard', () => {
  test('uses navigator.clipboard when it exists', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    stubNavigatorClipboard({ writeText });
    const execCommand = vi.fn();
    stubExecCommand(execCommand as unknown as () => boolean);

    await expect(copyTextToClipboard('hello')).resolves.toBe(true);

    expect(writeText).toHaveBeenCalledWith('hello');
    expect(execCommand).not.toHaveBeenCalled();
  });

  test('falls back to execCommand when navigator.clipboard is absent (insecure origin)', async () => {
    stubNavigatorClipboard(undefined);
    const execCommand = vi.fn().mockReturnValue(true);
    stubExecCommand(execCommand as unknown as () => boolean);

    await expect(copyTextToClipboard('curl example')).resolves.toBe(true);

    expect(execCommand).toHaveBeenCalledWith('copy');
    // The hidden staging textarea is cleaned up again.
    expect(document.querySelectorAll('textarea')).toHaveLength(0);
  });

  test('falls back to execCommand when writeText rejects', async () => {
    stubNavigatorClipboard({
      writeText: vi.fn().mockRejectedValue(new Error('denied')),
    });
    const execCommand = vi.fn().mockReturnValue(true);
    stubExecCommand(execCommand as unknown as () => boolean);

    await expect(copyTextToClipboard('text')).resolves.toBe(true);
    expect(execCommand).toHaveBeenCalledWith('copy');
  });

  test('returns false instead of throwing when no copy path exists', async () => {
    stubNavigatorClipboard(undefined);
    // jsdom leaves execCommand undefined: the legacy path is unavailable too.

    await expect(copyTextToClipboard('text')).resolves.toBe(false);
  });
});