export function loginSessionDevice(userAgent: string): {
  name: string;
  kind: "desktop" | "phone" | "tv" | "unknown";
} {
  const ua = userAgent.trim();
  if (!ua) return { name: "Unknown device", kind: "unknown" };
  const platform = /tvOS|AppleTV/i.test(ua)
    ? "Apple TV"
    : /Android.*TV|Android TV/i.test(ua)
      ? "Android TV"
      : /Android/i.test(ua)
        ? "Android"
        : /iPad/i.test(ua)
          ? "iPad"
          : /iPhone/i.test(ua)
            ? "iPhone"
            : /iOS/i.test(ua)
              ? "iOS"
              : /Windows/i.test(ua)
                ? "Windows"
                : /Macintosh|macOS|Mac OS X/i.test(ua)
                  ? "macOS"
                  : /Linux/i.test(ua)
                    ? "Linux"
                    : "";
  const app = /\bSilo\b/i.test(ua)
    ? "Silo app"
    : /Edg(?:e|A|iOS)?\//.test(ua)
      ? "Edge"
      : /(?:Firefox|FxiOS)\//.test(ua)
        ? "Firefox"
        : /(?:OPR|Opera)\//.test(ua)
          ? "Opera"
          : /(?:Chrome|CriOS|HeadlessChrome)\//.test(ua)
            ? "Chrome"
            : /Safari\//.test(ua)
              ? "Safari"
              : "";
  const kind = /TV/.test(platform)
    ? "tv"
    : /Android|iPhone|iPad|iOS/.test(platform)
      ? "phone"
      : platform
        ? "desktop"
        : "unknown";
  return {
    name: app
      ? platform
        ? `${app} on ${platform}`
        : app
      : platform
        ? `Device on ${platform}`
        : "Unknown device",
    kind,
  };
}

export function sessionLastSeen(value: string | null, now = Date.now()): string {
  if (!value) return "Last seen not recorded yet";
  const elapsed = Math.max(0, now - Date.parse(value));
  if (!Number.isFinite(elapsed)) return "Last seen not recorded yet";
  if (elapsed < 120_000) return "Active now";
  const minutes = Math.floor(elapsed / 60_000);
  return minutes < 60
    ? `Last seen ${minutes} min ago`
    : minutes < 1440
      ? `Last seen ${Math.floor(minutes / 60)} hr ago`
      : `Last seen ${Math.floor(minutes / 1440)} days ago`;
}
