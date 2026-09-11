import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "system_importV4",
  description: "นำเข้าข้อมูลคลินิกจากไฟล์ dump เข้า V4 (inspect → map → plan → run)",
};

/**
 * Resolves the theme before the first paint.
 *
 * This has to be a blocking inline script, not an effect: React runs after the browser has
 * already painted, so a dark page would flash white (or a light page flash black) on every
 * load. It is the standard cost of a themed app that renders on the server.
 *
 * No stored choice means follow the machine. A stored one always wins — see ThemeToggle.
 * The catch falls back to dark, which is what the tool looked like before there was a
 * choice, so a browser with storage blocked gets the old behaviour rather than a blank
 * unstyled page.
 */
const themeBootstrap = `
(function () {
  try {
    var stored = window.localStorage.getItem('importv4-theme');
    var theme = stored === 'light' || stored === 'dark'
      ? stored
      : (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
    document.documentElement.dataset.theme = theme;
  } catch (e) {
    document.documentElement.dataset.theme = 'dark';
  }
})();
`;

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    // suppressHydrationWarning: the script above adds data-theme to this element before
    // React hydrates, so the server HTML and the live DOM differ here on purpose.
    <html lang="th" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: themeBootstrap }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
