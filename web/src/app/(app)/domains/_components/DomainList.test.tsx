import { render, screen } from "../../../../test-utils/swr";
import { DomainList } from "./DomainList";
import type { DomainInfo } from "../../../components/onboarding/types";
import type { DashboardAgent } from "../../../components/types";

// DomainList is a thin mapper over DomainCard. These tests pin the two
// behaviors it owns: one card per domain, and the agent_count preference
// (server-computed count wins; client-side filter is the fallback).

function makeDomain(overrides: Partial<DomainInfo> = {}): DomainInfo {
  return {
    domain: "mail.example.com",
    verified: false,
    verification_token: "e2a-verify=abc123",
    dns_records: [],
    created_at: "2026-01-01T00:00:00Z",
    verified_at: null,
    ...overrides,
  };
}

function makeAgent(domain: string, email: string): DashboardAgent {
  return {
    domain,
    email,
    name: email.split("@")[0],
    domain_verified: true,
    created_at: "2026-01-20T00:00:00Z",
  } as DashboardAgent;
}

describe("DomainList", () => {
  it("renders one card per domain", () => {
    render(
      <DomainList
        domains={[
          makeDomain({ domain: "a.example.com" }),
          makeDomain({ domain: "b.example.com" }),
          makeDomain({ domain: "c.example.com" }),
        ]}
        agents={[]}
        onRefresh={jest.fn()}
      />,
    );
    expect(screen.getByText("a.example.com")).toBeInTheDocument();
    expect(screen.getByText("b.example.com")).toBeInTheDocument();
    expect(screen.getByText("c.example.com")).toBeInTheDocument();
  });

  it("renders nothing when there are no domains", () => {
    const { container } = render(
      <DomainList domains={[]} agents={[]} onRefresh={jest.fn()} />,
    );
    expect(container.firstElementChild).toBeEmptyDOMElement();
  });

  it("prefers the server-computed agent_count over the client-side filter", () => {
    render(
      <DomainList
        domains={[makeDomain({ agent_count: 5 })]}
        agents={[]}
        onRefresh={jest.fn()}
      />,
    );
    expect(screen.getByText("5 inboxes")).toBeInTheDocument();
  });

  it("falls back to filtering agents by domain when agent_count is absent", () => {
    render(
      <DomainList
        domains={[makeDomain()]}
        agents={[
          makeAgent("mail.example.com", "one@mail.example.com"),
          makeAgent("mail.example.com", "two@mail.example.com"),
          makeAgent("other.example.com", "other@other.example.com"),
        ]}
        onRefresh={jest.fn()}
      />,
    );
    expect(screen.getByText("2 inboxes")).toBeInTheDocument();
  });
});
