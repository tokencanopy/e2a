import { render, screen } from "@testing-library/react";
import TransactionalEmailApiPage, { metadata } from "./page";

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
    expect(metadata.description).toMatch(/receipts, verification messages, product notifications, and alerts/i);
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
