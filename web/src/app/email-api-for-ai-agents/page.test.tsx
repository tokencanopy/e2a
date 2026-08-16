import { render, screen } from "@testing-library/react";
import EmailApiForAgentsPage from "./page";

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
    return (
      <a href={href} {...props}>
        {children}
      </a>
    );
  };
});

describe("email API category page", () => {
  it("states the category and links to concrete workflows", () => {
    render(<EmailApiForAgentsPage />);

    expect(
      screen.getByRole("heading", { level: 1, name: "Build agents that work over email." }),
    ).toBeInTheDocument();
    expect(screen.getByText(/e2a gives every agent a real, authenticated inbox/i)).toBeInTheDocument();
    for (const slug of ["support-agent", "ai-receptionist", "scheduling-agent", "ecommerce-agent", "sales-agent", "procurement-agent"]) {
      expect(document.querySelector(`a[href="/use-cases/${slug}"]`)).toBeInTheDocument();
    }
  });

  it("renders the search-intent FAQ answers", () => {
    render(<EmailApiForAgentsPage />);
    expect(screen.getByText("What is an email API for AI agents?")).toBeInTheDocument();
    expect(screen.getByText("What is the difference between e2a and a transactional email API?")).toBeInTheDocument();
    expect(screen.getByText("How does e2a authenticate inbound email?")).toBeInTheDocument();
    expect(screen.getByText("What are the best ways to give an AI agent its own email address?")).toBeInTheDocument();
  });

  it("enumerates the ways to give an agent an email address", () => {
    render(<EmailApiForAgentsPage />);
    expect(
      screen.getByRole("heading", { level: 2, name: "Ways to give an AI agent its own email address." }),
    ).toBeInTheDocument();
    expect(screen.getByText(/a dedicated gateway is the only option where the agent owns a real address/i)).toBeInTheDocument();
    for (const option of ["Dedicated email gateway", "Connect an existing account", "Direct IMAP access", "Forward into an AI integration"]) {
      expect(screen.getByText(option)).toBeInTheDocument();
    }
    expect(screen.getByText("e2a, AgentMail")).toBeInTheDocument();
  });
});
