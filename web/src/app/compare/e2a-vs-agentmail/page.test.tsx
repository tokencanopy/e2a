import { render, screen } from "@testing-library/react";
import E2aVsAgentMailPage from "./page";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...props }: { href: string; children: React.ReactNode; [key: string]: unknown }) {
    return <a href={href} {...props}>{children}</a>;
  };
});

describe("e2a comparison page", () => {
  it("explains the three product categories", () => {
    render(<E2aVsAgentMailPage />);

    expect(screen.getByRole("heading", { level: 1, name: "e2a vs AgentMail vs transactional email APIs." })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Transactional APIs" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Agent inbox platforms" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Gateway infrastructure" })).toBeInTheDocument();
  });

  it("names the concrete transactional providers and policy distinction", () => {
    render(<E2aVsAgentMailPage />);

    for (const name of ["Amazon SES", "Resend", "Postmark", "Mailgun", "Twilio SendGrid"]) {
      expect(screen.getByText(name)).toBeInTheDocument();
    }
    expect(screen.getByRole("heading", { name: "Policy lives at the gateway." })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Read how e2a works →" })).toHaveAttribute("href", "/email-api-for-ai-agents");
  });

  it("renders comparison FAQs", () => {
    render(<E2aVsAgentMailPage />);
    expect(screen.getByText("What is the difference between e2a and AgentMail?")).toBeInTheDocument();
    expect(screen.getByText("Can e2a work with a transactional email provider?")).toBeInTheDocument();
  });
});
