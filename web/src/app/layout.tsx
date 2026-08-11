import type { Metadata } from "next";
import { Fraunces, Inter, JetBrains_Mono } from "next/font/google";
// @e2a/ui component classes (loft-*). Imported before globals.css so the app's
// globals.css stays authoritative for design tokens and the Tailwind theme.
import "@e2a/ui/styles.css";
import "./globals.css";
import { AuthProvider } from "./components/AuthProvider";
import { ThemeProvider } from "./components/ThemeProvider";
import { JsonLd } from "./components/JsonLd";
import { UmamiTracker } from "./components/UmamiTracker";
import { SITE_URL, SITE_NAME, GOOGLE_SITE_VERIFICATION } from "../lib/site";
import { organization, softwareApplication, website } from "../lib/jsonld";

const inter = Inter({
  variable: "--f-ui",
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
});

const jetbrainsMono = JetBrains_Mono({
  variable: "--f-mono",
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
});

const fraunces = Fraunces({
  variable: "--f-editorial",
  subsets: ["latin"],
  style: ["normal", "italic"],
  axes: ["opsz"],
});

const ROOT_TITLE = "e2a — Authenticated Email for AI Agents";
const ROOT_DESC =
  "Authenticated email gateway for AI agents: verified sender identity, SPF/DKIM, agent-to-agent routing, conversation threading. Send and receive real email from your agent with a signed webhook or WebSocket.";

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: {
    default: ROOT_TITLE,
    template: `%s — ${SITE_NAME}`,
  },
  description: ROOT_DESC,
  applicationName: SITE_NAME,
  keywords: [
    "email api for ai agents",
    "agent email gateway",
    "authenticated email webhook",
    "SPF DKIM AI agents",
    "agent-to-agent email",
    "conversation id email threading",
    "smtp relay for AI agents",
    "send email from python agent",
    "ai assistant email",
    "openclaw",
    "email for openclaw",
  ],
  authors: [{ name: SITE_NAME }],
  alternates: {
    canonical: "/",
  },
  openGraph: {
    title: ROOT_TITLE,
    description: ROOT_DESC,
    url: SITE_URL,
    siteName: SITE_NAME,
    type: "website",
    locale: "en_US",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: {
    card: "summary_large_image",
    title: ROOT_TITLE,
    description: ROOT_DESC,
    images: ["/og-image.png"],
  },
  // Google Search Console verification — only emitted when configured.
  ...(GOOGLE_SITE_VERIFICATION
    ? { verification: { google: GOOGLE_SITE_VERIFICATION } }
    : {}),
  robots: {
    index: true,
    follow: true,
    googleBot: {
      index: true,
      follow: true,
      "max-snippet": -1,
      "max-image-preview": "large",
      "max-video-preview": -1,
    },
  },
  icons: {
    icon: [{ url: "/favicon.ico", sizes: "any" }],
    apple: "/apple-touch-icon.png",
  },
};

// Site-wide structured data. The Organization node carries an @id that every
// other page's schema references, so publisher identity is stated once.
const rootJsonLd = [
  organization(),
  website(),
  softwareApplication(ROOT_DESC),
];

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className={`${inter.variable} ${jetbrainsMono.variable} ${fraunces.variable} antialiased`}
      suppressHydrationWarning
    >
      <body className="min-h-screen flex flex-col">
        <JsonLd data={rootJsonLd} />
        <UmamiTracker />
        <AuthProvider>
          <ThemeProvider>
            {children}
          </ThemeProvider>
        </AuthProvider>
      </body>
    </html>
  );
}
