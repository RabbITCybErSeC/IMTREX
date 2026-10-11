// Mock switch. A public build-time variable (only NEXT_PUBLIC_ prefixed ones are readable in the browser).
// Set NEXT_PUBLIC_MOCK=1 on Vercel to route the whole site through mocks, no backend needed.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
