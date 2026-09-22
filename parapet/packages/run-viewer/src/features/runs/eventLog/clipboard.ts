// navigator.clipboard requires a secure context, but the castle ingress is
// plain HTTP on an internal IP, so a real browser leaves it undefined there
// (CRI-284). Fall back to the legacy execCommand copy path, which also works
// on insecure origins; report the outcome instead of throwing so a copy
// affordance can never take the page down.
export async function copyTextToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Permission denied or unavailable: fall through to the legacy path.
    }
  }
  try {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.opacity = '0';
    document.body.appendChild(area);
    area.select();
    const copied = document.execCommand('copy');
    area.remove();
    return copied;
  } catch {
    return false;
  }
}