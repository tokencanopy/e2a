import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthProvider, useAuth } from "./AuthProvider";

function SignOutButton() {
  const { signOut } = useAuth();
  return <button onClick={() => void signOut()}>Sign out</button>;
}

describe("AuthProvider sign out", () => {
  beforeEach(() => {
    global.fetch = jest.fn(async () => ({ ok: false })) as unknown as typeof fetch;
    jest.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
  });

  afterEach(() => {
    jest.restoreAllMocks();
  });

  it("submits a native POST so cross-origin logout redirects can clear upstream cookies", async () => {
    render(
      <AuthProvider>
        <SignOutButton />
      </AuthProvider>,
    );

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));

    const form = document.querySelector('form[action="/api/auth/logout"]');
    expect(form).toHaveAttribute("method", "POST");
    expect(HTMLFormElement.prototype.submit).toHaveBeenCalledTimes(1);
    expect(global.fetch).toHaveBeenCalledWith("/api/auth/me", {
      credentials: "include",
    });
  });
});
