import { render, screen, waitFor } from "@testing-library/react";

// site.ts reads NEXT_PUBLIC_* at module load. Set the real hosted build
// identity before loading the component so this exercises the same config
// contract as the static dashboard build.
const originalSiteURL = process.env.NEXT_PUBLIC_SITE_URL;
const originalSignInURL = process.env.NEXT_PUBLIC_E2A_SIGN_IN_URL;
process.env.NEXT_PUBLIC_SITE_URL = "https://e2a.dev";
process.env.NEXT_PUBLIC_E2A_SIGN_IN_URL = "/api/auth/oidc/login";

// eslint-disable-next-line @typescript-eslint/no-require-imports
const { SignInLinks } = require("./SignInLinks") as typeof import("./SignInLinks");

describe("SignInLinks — hosted rollout", () => {
  afterAll(() => {
    if (originalSiteURL === undefined) delete process.env.NEXT_PUBLIC_SITE_URL;
    else process.env.NEXT_PUBLIC_SITE_URL = originalSiteURL;
    if (originalSignInURL === undefined)
      delete process.env.NEXT_PUBLIC_E2A_SIGN_IN_URL;
    else process.env.NEXT_PUBLIC_E2A_SIGN_IN_URL = originalSignInURL;
  });

  it("renders TokenCanopy and legacy Google doors with the same return_to", () => {
    const returnTo =
      "/oauth2/authorize?client_id=mcp_example&state=opaque-state";

    render(<SignInLinks returnTo={returnTo} />);

    expect(
      screen.getByRole("link", { name: "Sign in with TokenCanopy" }),
    ).toHaveAttribute(
      "href",
      "/api/auth/oidc/login?return_to=%2Foauth2%2Fauthorize%3Fclient_id%3Dmcp_example%26state%3Dopaque-state",
    );
    expect(
      screen.getByRole("link", { name: "Sign in with Google" }),
    ).toHaveAttribute(
      "href",
      "/api/auth/login?return_to=%2Foauth2%2Fauthorize%3Fclient_id%3Dmcp_example%26state%3Dopaque-state",
    );
  });

  it("preserves the same dashboard deep link on both doors", async () => {
    window.history.replaceState(null, "", "/reviews?id=msg_example");

    render(<SignInLinks preserveCurrentPaths={["/reviews"]} />);

    await waitFor(() => {
      expect(
        screen.getByRole("link", { name: "Sign in with TokenCanopy" }),
      ).toHaveAttribute(
        "href",
        "/api/auth/oidc/login?return_to=%2Freviews%3Fid%3Dmsg_example",
      );
      expect(
        screen.getByRole("link", { name: "Sign in with Google" }),
      ).toHaveAttribute(
        "href",
        "/api/auth/login?return_to=%2Freviews%3Fid%3Dmsg_example",
      );
    });
  });
});
