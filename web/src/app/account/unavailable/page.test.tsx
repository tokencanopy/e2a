import { render, screen } from "@testing-library/react";
import AccountUnavailablePage from "./page";
import { metadata } from "../layout";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

let searchParamsValue = new URLSearchParams();
jest.mock("next/navigation", () => ({
  useSearchParams: () => searchParamsValue,
}));

jest.mock("../../components/SignInLink", () => ({
  SignInLink: ({ children, href }: { children: React.ReactNode; href?: string }) => (
    <a href={href}>{children}</a>
  ),
}));

function renderWithCode(qs: string) {
  searchParamsValue = new URLSearchParams(qs);
  render(<AccountUnavailablePage />);
}

describe("/account/unavailable", () => {
  it("is kept out of search indexes", () => {
    expect(metadata.robots).toMatchObject({ index: false, follow: false });
  });

  it.each([
    ["registration_refused", /this sign-in can.t be used right now/i, /recently deleted or closed/i, false],
    ["purge_in_progress", /this account is being erased/i, /can.t be restored/i, false],
    ["temporarily_unavailable", /sign-in is temporarily unavailable/i, /try again in a few minutes/i, true],
    ["account_trashed", /this account is in the trash/i, /sign in again to restore it/i, true],
  ])("explains code=%s", (code, title, body, offersSignIn) => {
    renderWithCode(`code=${code}`);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(title);
    expect(screen.getByText(body)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /back to e2a home/i })).toHaveAttribute("href", "/");
    if (offersSignIn) {
      expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
    } else {
      expect(screen.queryByRole("link", { name: /sign in/i })).not.toBeInTheDocument();
    }
  });

  it.each(["", "code=something_new", "code=__proto__", "code=toString"])(
    "renders a generic message for %p",
    (qs) => {
      renderWithCode(qs);
      expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(/this account isn.t available/i);
      expect(screen.getByRole("link", { name: /back to e2a home/i })).toBeInTheDocument();
    },
  );

  it("never echoes the raw code or any account data", () => {
    renderWithCode("code=%3Cscript%3Eowner%40example.test");
    expect(document.body).not.toHaveTextContent("owner@example.test");
    expect(document.body).not.toHaveTextContent("<script>");
  });
});
