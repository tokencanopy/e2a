import { render } from "@testing-library/react";
import WebhookSecretsRedirect from "./page";

const mockReplace = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ replace: mockReplace }),
}));

beforeEach(() => mockReplace.mockClear());

describe("/webhook-secrets — back-compat redirect", () => {
  it("renders nothing and replaces the route with /webhooks", () => {
    const { container } = render(<WebhookSecretsRedirect />);
    expect(container.firstChild).toBeNull();
    expect(mockReplace).toHaveBeenCalledTimes(1);
    expect(mockReplace).toHaveBeenCalledWith("/webhooks");
  });
});
