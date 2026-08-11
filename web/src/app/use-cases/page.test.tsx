import { render, screen } from "@testing-library/react";
import UseCasesPage from "./page";

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

describe("use-case hub", () => {
  it("renders the canonical answer and all published workflow links", () => {
    render(<UseCasesPage />);

    expect(
      screen.getByRole("heading", { level: 1, name: "What can you build with e2a?" }),
    ).toBeInTheDocument();

    for (const slug of [
      "support-agent",
      "ai-receptionist",
      "scheduling-agent",
      "ecommerce-agent",
      "sales-agent",
      "recruiting-agent",
      "voice-agent",
      "procurement-agent",
    ]) {
      expect(document.querySelector(`a[href="/use-cases/${slug}"]`)).toBeInTheDocument();
    }
  });
});
