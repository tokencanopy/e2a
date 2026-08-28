import { render, screen } from "@testing-library/react";
import TransactionalEmailApiPage, {
  metadata,
  TransactionalEmailApiPageContent,
} from "./page";

jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...props
  }: {
    href: string;
    children: React.ReactNode;
    [key: string]: unknown;
  }) {
    return <a href={href} {...props}>{children}</a>;
  };
});

describe("transactional email API page", () => {
  it("makes the conventional application path explicit", () => {
    render(<TransactionalEmailApiPage />);

    expect(
      screen.getByRole("heading", {
        level: 1,
        name: "The open-source transactional email API for applications.",
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/No AI agent or agent framework is required/i),
    ).toBeInTheDocument();
    expect(
      screen.getAllByRole("link", { name: /Create a sender/i })[0],
    ).toHaveAttribute("href", "/get-started?step=address");
    expect(screen.queryByRole("link", { name: "View pricing" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View source" })).toHaveAttribute(
      "href",
      "https://github.com/tokencanopy/e2a",
    );
    expect(metadata.description).toMatch(/receipts, verification messages, product notifications, and alerts/i);
  });

  it("keeps secondary navigation out of the 320px mobile row", () => {
    render(<TransactionalEmailApiPage />);
    for (const name of ["Agent inboxes", "API docs"]) {
      expect(screen.getByRole("link", { name })).toHaveClass("hidden", "sm:inline");
    }
    expect(screen.getByRole("link", { name: "Create sender" })).not.toHaveClass("hidden");
  });

  it("links pricing only when the deployment provides a pricing route", () => {
    const { rerender } = render(
      <TransactionalEmailApiPageContent pricingPath="" />,
    );
    expect(screen.queryByRole("link", { name: "View pricing" })).not.toBeInTheDocument();

    rerender(<TransactionalEmailApiPageContent pricingPath="/pricing" />);
    expect(screen.getByRole("link", { name: "View pricing" })).toHaveAttribute(
      "href",
      "/pricing",
    );
  });

  it("explains the existing agent resource to application developers", () => {
    render(<TransactionalEmailApiPage />);
    expect(
      screen.getByText(/each sending identity is represented by an agent resource/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/treat it as the sender and optional inbox/i),
    ).toBeInTheDocument();
  });

  it("renders a direct general transactional API answer", () => {
    render(<TransactionalEmailApiPage />);
    expect(
      screen.getByText("Can e2a be used as a general transactional email API?"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Agent inboxes, inbound email, threading, and human approval are additional capabilities—not requirements/i),
    ).toBeInTheDocument();
  });
});
