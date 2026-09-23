import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import InfrastructureSettings from "./InfrastructureSettings";
import { OPSLOG_BUCKET_POLICIES_KEY } from "./logRetentionPolicy";

window.HTMLElement.prototype.hasPointerCapture ??= () => false;
window.HTMLElement.prototype.setPointerCapture ??= () => {};
window.HTMLElement.prototype.releasePointerCapture ??= () => {};
window.HTMLElement.prototype.scrollIntoView ??= () => {};

const settingsFormMock = vi.fn();
const useCheckAdminSettingsConnectionMock = vi.fn();
const createStorageTransitionMock = vi.fn();
const cancelStorageTransitionMock = vi.fn();
const storageTransitionCapabilitiesMock = vi.fn();
const sourceHealthMock = vi.fn();
const taskJobsMock = vi.fn();

// Most cases drive the page from a hand-written form so a single render can
// describe any staged state. The cases that have to prove a value reaches the
// server flip this and run the real hook instead.
const { realForm, serverSettings, sensitiveStatus, updateSettingsMock } = vi.hoisted(() => ({
  realForm: { enabled: false },
  serverSettings: { current: {} as Record<string, string> },
  sensitiveStatus: { current: { configured: [] as string[], managed_by_env: [] as string[] } },
  updateSettingsMock: vi.fn(),
}));

vi.mock("@/hooks/useSettingsForm", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/hooks/useSettingsForm")>();
  return {
    useSettingsForm: (options: { keys: string[] }) =>
      realForm.enabled ? actual.useSettingsForm(options) : settingsFormMock(options),
  };
});

// Mirrors the server registry's restart prefixes: every artwork/s3/redis/database/
// userdb key restarts; nothing else on this page does. Tests that need a
// different registry override the mock.
const restartKeysMock = vi.fn(() => ({
  has: (key: string) => /^(artwork|s3|redis|database|userdb)\./.test(key),
}));

vi.mock("@/hooks/useRestartKeys", () => ({
  useRestartKeys: () => restartKeysMock(),
}));

vi.mock("@/hooks/queries/admin/settings", () => ({
  useCheckAdminSettingsConnection: (...args: unknown[]) =>
    useCheckAdminSettingsConnectionMock(...args),
  useAdminServerSettings: () => ({ data: serverSettings.current, isLoading: false }),
  useAdminSensitiveStatus: () => ({ data: sensitiveStatus.current, isError: false }),
  useUpdateServerSettings: () => ({ mutateAsync: updateSettingsMock, isPending: false }),
  useAdminServerStatus: () => ({ data: serverStatus.current }),
  useCreateStorageTransition: () => ({
    mutateAsync: createStorageTransitionMock,
    isPending: false,
  }),
  useStorageTransitionCapabilities: () => storageTransitionCapabilitiesMock(),
  useStorageTransitionSourceHealth: (...args: unknown[]) => sourceHealthMock(...args),
  useCancelStorageTransition: () => ({
    mutateAsync: cancelStorageTransitionMock,
    isPending: false,
  }),
}));

vi.mock("@/hooks/queries/admin/taskJobs", () => ({
  useAdminTaskJobs: () => taskJobsMock(),
}));

const serverStatus: {
  current:
    | { artwork_storage?: { backend?: string; locked: boolean; private_locked?: boolean } }
    | undefined;
} = {
  current: undefined,
};

useCheckAdminSettingsConnectionMock.mockReturnValue({ isPending: false, mutateAsync: vi.fn() });
storageTransitionCapabilitiesMock.mockReturnValue({
  data: { state: "available", allowed: true },
  isPending: false,
  isError: false,
});
function mockUncheckedSourceHealth() {
  sourceHealthMock.mockReturnValue({
    data: undefined,
    isPending: false,
    isFetching: false,
    isError: false,
    refetch: vi.fn(),
  });
}
mockUncheckedSourceHealth();
taskJobsMock.mockReturnValue({ data: [], isFetching: false, refetch: vi.fn() });

type FormOverrides = Partial<Record<string, unknown>>;

function mockForm(overrides: FormOverrides = {}) {
  const form = {
    isLoading: false,
    getValue: (key: string) => (key === "s3.public_url_auth" ? "presigned" : ""),
    getPersistedValue: () => "",
    setValue: vi.fn(),
    resetValue: vi.fn(),
    dirtyCount: 0,
    dirtyKeys: [],
    isDirty: () => false,
    isClearStaged: () => false,
    save: vi.fn(),
    discard: vi.fn(),
    isSaving: false,
    restartRequired: false,
    sensitiveConfigured: [],
    sensitiveManagedByEnv: [],
    sensitiveStatusReady: true,
    sensitiveStatusError: false,
    buildConnectionCheckRequest: vi.fn(),
    ...overrides,
  };
  settingsFormMock.mockReturnValue(form);
  return form;
}

describe("InfrastructureSettings", () => {
  afterEach(() => {
    serverStatus.current = undefined;
    sourceHealthMock.mockReset();
    mockUncheckedSourceHealth();
    taskJobsMock.mockReset();
    taskJobsMock.mockReturnValue({ data: [], isFetching: false, refetch: vi.fn() });
    storageTransitionCapabilitiesMock.mockReset();
    storageTransitionCapabilitiesMock.mockReturnValue({
      data: { state: "available", allowed: true },
      isPending: false,
      isError: false,
    });
  });

  it("renders every field group heading", () => {
    mockForm();

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    for (const heading of [
      "Storage",
      "Redis",
      "Public storage",
      "Private storage",
      "Database",
      "Logs",
    ]) {
      expect(markup).toContain(heading);
    }
  });

  it("keeps the backend editable until artwork has been stored", () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: false } };
    mockForm();
    render(<InfrastructureSettings />);
    expect(screen.getByRole("combobox", { name: "Backend" })).toBeEnabled();
    expect(screen.queryByText(/Locked to/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Local storage path")).toBeEnabled();
  });

  it("opens the managed transition when a locked S3 backend changes", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({ getValue: (key: string) => (key === "artwork.storage_backend" ? "auto" : "") });
    render(<InfrastructureSettings />);
    const backend = screen.getByRole("combobox", { name: "Backend" });
    expect(backend).toBeEnabled();
    expect(backend).toHaveTextContent("S3");
    expect(screen.queryByText(/Locked to S3/)).not.toBeInTheDocument();
    expect(screen.getByText(/Choose Automatic or Local disk/)).toBeInTheDocument();
    expect(screen.getByLabelText("Local storage path")).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Change storage" })).not.toBeInTheDocument();

    await userEvent.click(backend);
    await userEvent.click(screen.getByRole("option", { name: "Automatic" }));
    expect(screen.getByRole("dialog", { name: "Change artwork storage" })).toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Target" })).not.toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Target" })).toHaveTextContent(
      "Local disk (default Silo behavior)",
    );
    expect(screen.queryByText("Configured S3 location")).not.toBeInTheDocument();
    serverStatus.current = undefined;
  });

  it("does not open a managed transition when capability discovery is unavailable", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    storageTransitionCapabilitiesMock.mockReturnValue({
      data: { state: "unsupported", allowed: false },
      isPending: false,
      isError: false,
    });
    const form = mockForm({
      getValue: (key: string) => (key === "artwork.storage_backend" ? "s3" : ""),
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("combobox", { name: "Backend" }));
    await userEvent.click(screen.getByRole("option", { name: "Local disk" }));

    expect(
      screen.queryByRole("dialog", { name: "Change artwork storage" }),
    ).not.toBeInTheDocument();
    expect(form.setValue).not.toHaveBeenCalledWith("artwork.storage_backend", "local");
  });

  it("queues an explicit start-fresh transition back to local storage", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    const form = mockForm({
      getValue: (key: string) =>
        key === "artwork.storage_backend"
          ? "s3"
          : key === "artwork.local_path"
            ? "/srv/silo/artwork"
            : "",
    });
    createStorageTransitionMock.mockResolvedValueOnce({ job: { id: "transition-1" } });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("combobox", { name: "Backend" }));
    await userEvent.click(screen.getByRole("option", { name: "Local disk" }));

    expect(sourceHealthMock).toHaveBeenCalledWith(true, true);
    expect(
      screen.getByText("Backfill Metadata Images downloads from saved provider URLs, not old S3."),
    ).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: /Start fresh/ }));
    await userEvent.click(screen.getByRole("button", { name: "Queue transition" }));

    await waitFor(() =>
      expect(createStorageTransitionMock).toHaveBeenCalledWith({
        policy: "start_fresh",
        values: {
          "artwork.storage_backend": "local",
          "artwork.local_path": "/srv/silo/artwork",
        },
      }),
    );
    expect(form.resetValue).toHaveBeenCalledWith("artwork.storage_backend");
    expect(form.resetValue).toHaveBeenCalledWith("artwork.local_path");
    expect(form.discard).not.toHaveBeenCalled();
    serverStatus.current = undefined;
  });

  it("disables copy policies when the current S3 source is unreachable", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    const refetch = vi.fn();
    sourceHealthMock.mockReturnValue({
      data: {
        current_backend: "s3",
        reachable: false,
        public_configured: true,
        public_reachable: false,
        private_configured: true,
        private_reachable: false,
        message: "Current public and private S3 storage are unreachable.",
      },
      isPending: false,
      isFetching: false,
      isError: false,
      refetch,
    });
    mockForm({
      getValue: (key: string) =>
        key === "artwork.storage_backend"
          ? "s3"
          : key === "artwork.local_path"
            ? "/srv/silo/artwork"
            : "",
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("combobox", { name: "Backend" }));
    await userEvent.click(screen.getByRole("option", { name: "Local disk" }));

    expect(
      within(screen.getByRole("alert")).getByText(
        /Current public and private S3 storage are unreachable/i,
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Preserve personal uploads/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Migrate everything/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Start fresh/ })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Queue transition" })).toBeDisabled();

    await userEvent.click(screen.getByRole("radio", { name: /Start fresh/ }));
    expect(screen.getByRole("button", { name: "Queue transition" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Retry check" }));
    expect(refetch).toHaveBeenCalledOnce();
    serverStatus.current = undefined;
  });

  it("renders the page header on its own, with no description or status strip", () => {
    mockForm();

    render(<InfrastructureSettings />);

    expect(
      screen.getByRole("heading", { level: 1, name: "Storage & Database" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Where Silo keeps its data. Changes here take effect after a restart."),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Redis not configured")).not.toBeInTheDocument();
    expect(screen.queryByText("No public bucket set")).not.toBeInTheDocument();
  });

  it("claims a restart only for the groups whose settings actually need one", () => {
    mockForm();

    render(<InfrastructureSettings />);

    // No page-wide claim: the Logs group applies live, and a blanket line
    // would tell an admin to restart for it.
    expect(
      screen.queryByText("Changes on this page apply after a restart."),
    ).not.toBeInTheDocument();
    // Artwork, Redis, both storage buckets, and Database each say it once; their
    // fields drop the per-field chips.
    expect(screen.getAllByText(/Changes apply after a restart/)).toHaveLength(5);
    expect(screen.queryAllByLabelText("Takes effect after a server restart")).toHaveLength(0);
    const logsGroup = within(screen.getByRole("group", { name: "Logs" }));
    expect(logsGroup.queryByText(/Changes apply after a restart/)).not.toBeInTheDocument();
  });

  it("demotes a group to per-field chips when one of its keys stops needing a restart", () => {
    // The same page against a future registry where the Redis URL hot-reloads:
    // the group-level claim must disappear on its own rather than stay wrong.
    restartKeysMock.mockReturnValueOnce({
      has: (key: string) => /^(s3|database|userdb)\./.test(key),
    });
    mockForm();

    render(<InfrastructureSettings />);

    const redisGroup = within(screen.getByRole("group", { name: "Redis" }));
    expect(redisGroup.queryByText(/Changes apply after a restart/)).not.toBeInTheDocument();
  });

  it("says what each bucket holds", () => {
    mockForm();

    render(<InfrastructureSettings />);

    expect(
      screen.getByText(
        /Files clients download directly: cached artwork, uploaded posters, and branding images/,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /Files only the server reads: profile avatars, diagnostics bundles, and catalog seed artifacts/,
      ),
    ).toBeInTheDocument();
  });

  it("puts units beside the control rather than in the label", () => {
    mockForm();

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    expect(markup).toContain("Delete log entries older than");
    expect(markup).not.toContain("Delete log entries older than (days)");
    expect(markup).not.toContain("Maximum log size (MB)");
  });

  it("manages the merged database, storage and log keys in one form", () => {
    mockForm();

    renderToStaticMarkup(<InfrastructureSettings />);

    const calls = settingsFormMock.mock.calls as [{ keys: string[] }][];
    const keys = calls[calls.length - 1]?.[0].keys ?? [];
    expect(keys).toEqual(expect.arrayContaining(["redis.url", "database.max_connections"]));
    expect(keys).toEqual(
      expect.arrayContaining(["s3.public_bucket", "s3.private_bucket", OPSLOG_BUCKET_POLICIES_KEY]),
    );
    // Provider caching remains on Library & Metadata. This page selects storage.
    expect(keys).not.toContain("metadata.cache_images");
    // The disabled Litestream storage tab is gone; its keys keep working through the API.
    expect(keys.filter((key) => key.startsWith("s3.user_db_"))).toEqual([]);
  });

  it("shows only essential controls until Advanced is opened", () => {
    mockForm();

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    expect(markup).toContain("Use Redis");
    expect(markup).toContain("Endpoint");
    expect(markup).toContain("Bucket");
    expect(markup).toContain("Check Connection");
    expect(markup).toContain("Maximum log entries");
    // Advanced, so not rendered while collapsed.
    expect(markup).not.toContain("Region");
    expect(markup).not.toContain("Maximum Postgres connections");
    expect(markup).not.toContain("Record one allowed check in every");
    expect(markup).not.toContain("Per-area limits");
    // Removed entirely.
    expect(markup).not.toContain("User DB");
    expect(markup).not.toContain("Not currently in use");
  });

  it("keeps the Redis connection check available when REDIS_URL comes from the environment", () => {
    mockForm({
      sensitiveConfigured: ["redis.url"],
      sensitiveManagedByEnv: ["redis.url"],
    });

    render(<InfrastructureSettings />);

    const redisGroup = within(screen.getByRole("group", { name: "Redis" }));
    // The value stays read-only — only writes are refused for env-managed keys —
    // but the check runs against the value the server merged from REDIS_URL.
    expect(redisGroup.getByLabelText("Connection URL")).toBeDisabled();
    expect(redisGroup.getByRole("button", { name: "Check Connection" })).toBeEnabled();
  });

  it("renders Check Connection as a filled button rather than flat text", () => {
    mockForm();

    render(<InfrastructureSettings />);

    for (const button of screen.getAllByRole("button", { name: "Check Connection" })) {
      expect(button).toHaveAttribute("data-variant", "secondary");
    }
  });

  it("opens an Advanced section while one of its fields is unsaved", () => {
    mockForm({ isDirty: (key: string) => key === "database.max_connections", dirtyCount: 1 });

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    expect(markup).toContain("Maximum Postgres connections");
    expect(markup).toContain("Advanced · 2 settings");
  });

  it("warns that the first artwork write locks a public storage identity field", () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: false } };
    mockForm({
      isDirty: (key: string) => key === "s3.public_bucket",
      dirtyCount: 1,
      getValue: (key: string) => (key === "s3.public_bucket" ? "new-artwork" : ""),
    });

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    expect(markup).toContain("Storage location change");
    expect(markup).toContain("settings-field-note");
    expect(markup).toContain("The first artwork write records this location");
    serverStatus.current = undefined;
  });

  it("explains the managed transition when a locked S3 identity field is edited", () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({
      isDirty: (key: string) => key === "s3.public_bucket",
      dirtyCount: 1,
      getValue: (key: string) => (key === "s3.public_bucket" ? "new-artwork" : ""),
    });

    const markup = renderToStaticMarkup(<InfrastructureSettings />);

    expect(markup).toContain("Storage location change");
    expect(markup).toContain("Saving this location opens a managed transition");
    expect(markup).toContain("Review transition");
    serverStatus.current = undefined;
  });

  it("saves unrelated edits before opening an S3 location transition", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    const save = vi.fn().mockResolvedValue(undefined);
    mockForm({
      dirtyCount: 2,
      dirtyKeys: ["s3.public_bucket", "server.log_level"],
      isDirty: (key: string) => key === "s3.public_bucket" || key === "server.log_level",
      save,
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "s3.public_bucket") return "new-artwork";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    expect(save).toHaveBeenCalledWith(["server.log_level"]);
    expect(screen.getByRole("dialog", { name: "Change artwork storage" })).toBeVisible();
  });

  it("opens an S3-to-S3 transition when a locked endpoint or bucket is saved", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    const form = mockForm({
      dirtyCount: 2,
      isDirty: (key: string) => key === "s3.public_endpoint" || key === "s3.public_bucket",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "s3.public_endpoint") return "https://new-s3.example";
        if (key === "s3.public_bucket") return "new-artwork";
        if (key === "s3.private_endpoint") return "https://private.example";
        if (key === "s3.private_bucket") return "private-artifacts";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    createStorageTransitionMock.mockResolvedValueOnce({ job: { id: "transition-s3" } });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change artwork storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "New S3 location",
    );
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "new-artwork · https://new-s3.example",
    );
    await userEvent.click(within(dialog).getByRole("button", { name: "Queue transition" }));

    await waitFor(() =>
      expect(createStorageTransitionMock).toHaveBeenCalledWith({
        policy: "preserve_uploads",
        values: {
          "artwork.storage_backend": "s3",
          "s3.public_endpoint": "https://new-s3.example",
          "s3.public_bucket": "new-artwork",
        },
      }),
    );
    expect(form.resetValue).toHaveBeenCalledWith("s3.public_endpoint");
    expect(form.resetValue).toHaveBeenCalledWith("s3.public_bucket");
    serverStatus.current = undefined;
  });

  it("shows the kept endpoint during a bucket-only change", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({
      dirtyCount: 1,
      isDirty: (key: string) => key === "s3.public_bucket",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "s3.public_endpoint") return "https://s3.example";
        if (key === "s3.public_bucket") return "new-artwork";
        if (key === "s3.private_endpoint") return "https://private.example";
        if (key === "s3.private_bucket") return "private-artifacts";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    expect(screen.getByRole("group", { name: "Target" })).toHaveTextContent(
      "new-artwork · https://s3.example",
    );
    serverStatus.current = undefined;
  });

  it("describes a private-only S3 transition without public artwork claims", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({
      dirtyCount: 2,
      isDirty: (key: string) => key === "s3.private_endpoint" || key === "s3.private_bucket",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "s3.public_bucket") return "public-artwork";
        if (key === "s3.private_endpoint") return "https://private.example";
        if (key === "s3.private_bucket") return "private-new";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change private storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "private-new · https://private.example",
    );
    expect(within(dialog).getByRole("radio", { name: /Preserve profile avatars/ })).toBeVisible();
    expect(
      within(dialog).getByText(/Preserve profile avatars and Start fresh leave/),
    ).toBeVisible();
    expect(
      within(dialog).getByText(/Existing profile avatars will appear missing after the switch/),
    ).toBeVisible();
    expect(within(dialog).queryByText(/Provider artwork is rebuilt/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText(/Backfill Metadata Images/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText(/NFO\/sidecar/)).not.toBeInTheDocument();
    expect(within(dialog).getByText(/reconciled in the background after restart/)).toBeVisible();
  });

  it("describes public-only and combined S3 changes as artwork transitions", async () => {
    for (const changedKeys of [
      new Set(["s3.public_bucket"]),
      new Set(["s3.public_bucket", "s3.private_bucket"]),
    ]) {
      serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
      mockForm({
        dirtyCount: changedKeys.size,
        isDirty: (key: string) => changedKeys.has(key),
        getValue: (key: string) => {
          if (key === "artwork.storage_backend") return "s3";
          if (key === "s3.public_bucket") return "public-new";
          if (key === "s3.private_bucket") return "private-new";
          if (key === "s3.public_url_auth") return "presigned";
          return "";
        },
      });
      const view = render(<InfrastructureSettings />);
      await userEvent.click(screen.getByRole("button", { name: "Review transition" }));
      const dialog = screen.getByRole("dialog", { name: "Change artwork storage" });
      expect(dialog).toBeVisible();
      expect(screen.getByRole("radio", { name: /Preserve personal uploads/ })).toBeVisible();
      // Avatars move only with the private location.
      expect(
        within(dialog).getByText(
          changedKeys.has("s3.private_bucket")
            ? /profile avatars, and downloaded subtitles/
            : /posters, and downloaded subtitles\. Provider/,
        ),
      ).toBeVisible();
      expect(within(dialog).getByText(/including downloaded subtitles/)).toBeVisible();
      if (changedKeys.has("s3.private_bucket")) {
        expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
          "Private bucket: private-new",
        );
      }
      view.unmount();
    }
  });

  it("requires private S3 before offering avatar-preserving copy policies", async () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    mockForm({
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "local";
        if (key === "s3.public_bucket") return "public-artwork";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("combobox", { name: "Backend" }));
    await userEvent.click(screen.getByRole("option", { name: "S3" }));

    expect(screen.getByRole("radio", { name: /Preserve personal uploads/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /Migrate everything/ })).toBeDisabled();
    expect(screen.getAllByText(/Configure a private S3 bucket/)).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Queue transition" })).toBeDisabled();
    await userEvent.click(screen.getByRole("radio", { name: /Start fresh/ }));
    expect(screen.getByRole("button", { name: "Queue transition" })).toBeEnabled();
    serverStatus.current = undefined;
  });

  it("routes a private bucket added to a locked local install through a transition", async () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    const form = mockForm({
      dirtyCount: 2,
      dirtyKeys: ["s3.private_endpoint", "s3.private_bucket"],
      isDirty: (key: string) => key === "s3.private_endpoint" || key === "s3.private_bucket",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "local";
        if (key === "artwork.local_path") return "/srv/silo/artwork";
        if (key === "s3.private_endpoint") return "https://private.example";
        if (key === "s3.private_bucket") return "private-new";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    createStorageTransitionMock.mockResolvedValueOnce({ job: { id: "transition-private" } });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change private storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "Private bucket: private-new · https://private.example",
    );
    expect(within(dialog).queryByLabelText("Local artwork path")).not.toBeInTheDocument();
    expect(within(dialog).queryByText(/Backfill Metadata Images/)).not.toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("radio", { name: /Migrate all private data/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Queue transition" }));

    await waitFor(() =>
      expect(createStorageTransitionMock).toHaveBeenCalledWith({
        policy: "migrate_all",
        values: {
          "artwork.storage_backend": "local",
          "artwork.local_path": "/srv/silo/artwork",
          "s3.private_endpoint": "https://private.example",
          "s3.private_bucket": "private-new",
        },
      }),
    );
    expect(form.save).not.toHaveBeenCalled();
  });

  it("describes removing a local install's private bucket as a move to local disk", async () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    mockForm({
      dirtyCount: 1,
      dirtyKeys: ["s3.private_bucket"],
      isDirty: (key: string) => key === "s3.private_bucket",
      getPersistedValue: (key: string) => (key === "s3.private_bucket" ? "private-old" : ""),
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "local";
        if (key === "artwork.local_path") return "/srv/silo/artwork";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change private storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "kept on local disk",
    );
    // The old private bucket is the copy source, so its reachability is checked.
    expect(sourceHealthMock).toHaveBeenCalledWith(true, true);
  });

  it("saves a private field that was edited back to its stored value", () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    mockForm({
      dirtyCount: 1,
      dirtyKeys: ["s3.private_bucket"],
      isDirty: (key: string) => key === "s3.private_bucket",
      getPersistedValue: (key: string) => (key === "s3.private_bucket" ? "private" : ""),
      getValue: (key: string) => (key === "s3.private_bucket" ? "private " : ""),
    });
    render(<InfrastructureSettings />);

    expect(screen.getByRole("button", { name: "Save" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Review transition" })).not.toBeInTheDocument();
  });

  it("warns that an S3 install without a private bucket has nowhere to keep private data", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({
      dirtyCount: 2,
      isDirty: (key: string) => key === "s3.private_endpoint" || key === "s3.private_bucket",
      getPersistedValue: (key: string) =>
        key === "s3.private_bucket"
          ? "private-old"
          : key === "s3.private_endpoint"
            ? "https://private.example"
            : "",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "s3.public_bucket") return "public-artwork";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change private storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "catalog artifacts become unavailable",
    );
    expect(within(dialog).getByRole("group", { name: "Target" })).not.toHaveTextContent(
      "local disk",
    );
  });

  it("does not queue a private bucket without its endpoint", async () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    mockForm({
      dirtyCount: 1,
      isDirty: (key: string) => key === "s3.private_bucket",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "local";
        if (key === "artwork.local_path") return "/srv/silo/artwork";
        if (key === "s3.private_bucket") return "private-new";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));

    const dialog = screen.getByRole("dialog", { name: "Change private storage" });
    expect(within(dialog).getByRole("group", { name: "Target" })).toHaveTextContent(
      "private-new · endpoint not set",
    );
    expect(within(dialog).getByText(/Enter the private storage endpoint/)).toBeVisible();
    expect(within(dialog).getByRole("button", { name: "Queue transition" })).toBeDisabled();
  });

  it("says Start fresh leaves S3 data behind when disabling S3", async () => {
    serverStatus.current = { artwork_storage: { backend: "s3", locked: true } };
    mockForm({
      getPersistedValue: (key: string) => (key === "s3.private_bucket" ? "private" : ""),
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "s3";
        if (key === "artwork.local_path") return "/srv/silo/artwork";
        if (key === "s3.public_url_auth") return "presigned";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("combobox", { name: "Backend" }));
    await userEvent.click(screen.getByRole("option", { name: "Local disk" }));
    await userEvent.click(screen.getByRole("radio", { name: /Start fresh/ }));

    expect(screen.getByText(/Start fresh copies nothing/)).toBeVisible();
    await userEvent.click(screen.getByRole("radio", { name: /Migrate everything/ }));
    expect(screen.getByText(/is copied to local disk/)).toBeVisible();
  });

  it("saves location edits that name the same store", () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    const persisted: Record<string, string> = {
      "s3.private_endpoint": "https://private.example",
      "s3.private_bucket": "private",
      "s3.private_key_prefix": "ops",
    };
    const edited: Record<string, string> = {
      "s3.private_endpoint": "https://PRIVATE.example/",
      "s3.private_bucket": "Private",
      "s3.private_key_prefix": "ops/",
    };
    mockForm({
      dirtyCount: 3,
      dirtyKeys: Object.keys(edited),
      isDirty: (key: string) => key in edited,
      getPersistedValue: (key: string) => persisted[key] ?? "",
      getValue: (key: string) => edited[key] ?? "",
    });
    render(<InfrastructureSettings />);

    expect(screen.getByRole("button", { name: "Save" })).toBeVisible();
  });

  it("saves a leftover private prefix when there is no private bucket", () => {
    serverStatus.current = { artwork_storage: { backend: "local", locked: true } };
    mockForm({
      dirtyCount: 1,
      isDirty: (key: string) => key === "s3.private_key_prefix",
      getPersistedValue: (key: string) => (key === "s3.private_key_prefix" ? "ops" : ""),
      getValue: () => "",
    });
    render(<InfrastructureSettings />);

    expect(screen.getByRole("button", { name: "Save" })).toBeVisible();
  });

  it("locks only the private bucket when it alone holds data", async () => {
    serverStatus.current = {
      artwork_storage: { backend: "local", locked: false, private_locked: true },
    };
    mockForm({
      dirtyCount: 1,
      isDirty: (key: string) => key === "s3.private_bucket",
      getPersistedValue: (key: string) =>
        key === "s3.private_bucket"
          ? "private-old"
          : key === "s3.private_endpoint"
            ? "https://private.example"
            : "",
      getValue: (key: string) => {
        if (key === "artwork.storage_backend") return "local";
        if (key === "artwork.local_path") return "/srv/silo/artwork";
        if (key === "s3.private_endpoint") return "https://private.example";
        if (key === "s3.private_bucket") return "private-new";
        return "";
      },
    });
    render(<InfrastructureSettings />);

    expect(screen.getByLabelText("Local storage path")).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Review transition" }));
    expect(screen.getByRole("dialog", { name: "Change private storage" })).toBeVisible();
  });

  it("does not show a historical completed transition or flash its refresh action", () => {
    taskJobsMock.mockReturnValue({
      data: [{ id: "done", status: "completed", message: "" }],
      isFetching: true,
      refetch: vi.fn(),
    });
    mockForm();

    render(<InfrastructureSettings />);

    expect(screen.queryByText(/Storage transition:/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
  });

  it("shows a stable dismissible failed transition", async () => {
    taskJobsMock.mockReturnValue({
      data: [
        {
          id: "failed-transition",
          status: "failed",
          message: "Storage transition failed",
          error_message: "Target bucket is unreachable",
        },
      ],
      isFetching: true,
      refetch: vi.fn(),
    });
    mockForm();

    render(<InfrastructureSettings />);

    expect(screen.getByText("Storage transition failed")).toBeVisible();
    expect(screen.getByText("Target bucket is unreachable")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("Target bucket is unreachable")).not.toBeInTheDocument();
  });

  it("shows a dismissible manual-restart alert without showing ordinary completed jobs", async () => {
    taskJobsMock.mockReturnValue({
      data: [
        {
          id: "manual-restart",
          status: "completed",
          message:
            "Storage transition committed; automatic restart unavailable — restart Silo manually",
          result_payload: { manual_restart_required: true },
        },
      ],
      isFetching: false,
      refetch: vi.fn(),
    });
    mockForm();

    render(<InfrastructureSettings />);

    expect(screen.getByText("Manual restart required")).toBeVisible();
    expect(screen.getByText(/Restart the server to activate the new storage safely/)).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("Manual restart required")).not.toBeInTheDocument();
  });

  it("ignores manual-restart wording when the structured flag is cleared", () => {
    taskJobsMock.mockReturnValue({
      data: [
        {
          id: "restarted",
          status: "completed",
          message: "restart Silo manually",
          result_payload: { manual_restart_required: false },
        },
      ],
      isFetching: false,
      refetch: vi.fn(),
    });
    mockForm();
    render(<InfrastructureSettings />);
    expect(screen.queryByText("Manual restart required")).not.toBeInTheDocument();
  });

  it("shows static post-restart recovery state and its last error", () => {
    sourceHealthMock.mockReturnValue({
      data: {
        reachable: true,
        public_configured: true,
        public_reachable: true,
        private_configured: false,
        private_reachable: true,
        message: "",
        recovery_pending: true,
        recovery_state: "waiting_retry",
        recovery_error: "temporary S3 outage",
        recovery_progress_percent: 42,
        recovery_progress_message: "Reconciliation paused; waiting to retry",
      },
      isPending: false,
      isFetching: true,
      isError: false,
      refetch: vi.fn(),
    });
    mockForm();
    render(<InfrastructureSettings />);
    expect(sourceHealthMock).toHaveBeenCalledWith(false, true);
    expect(sourceHealthMock).toHaveBeenCalledWith(true, false);
    expect(screen.getByText("Storage recovery: waiting retry")).toBeVisible();
    expect(screen.getByText("temporary S3 outage")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Refresh" })).not.toBeInTheDocument();
  });

  it("keeps refresh stable while an active transition is background-polled", () => {
    taskJobsMock.mockReturnValue({
      data: [
        {
          id: "running",
          status: "running",
          message: "Copying artwork",
          progress_current: 2,
          progress_total: 10,
        },
      ],
      isFetching: true,
      refetch: vi.fn(),
    });
    mockForm();

    render(<InfrastructureSettings />);

    expect(screen.getByText("Storage transition: running")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Refresh" })).toBeEnabled();
  });

  it("keeps a saved credential when its input is emptied", async () => {
    const form = mockForm({
      sensitiveConfigured: ["s3.public_access_key", "s3.public_secret_key"],
      getValue: (key: string) =>
        key === "s3.public_access_key" ? "draft" : key === "s3.public_url_auth" ? "presigned" : "",
    });

    render(<InfrastructureSettings />);

    // No Replace step: the saved credential is a masked, always-editable input.
    const publicGroup = within(screen.getByRole("group", { name: "Public storage" }));
    const input = publicGroup.getByLabelText("Access Key");
    expect(input).toHaveAttribute("type", "password");
    expect(input).toHaveAttribute("placeholder", "••••••••••••");
    expect(publicGroup.queryByRole("button", { name: /Replace/ })).not.toBeInTheDocument();

    // Deleting the draft means "keep the saved secret", never "clear it".
    await userEvent.clear(input);
    expect(form.resetValue).toHaveBeenCalledWith("s3.public_access_key");
    expect(form.setValue).not.toHaveBeenCalledWith("s3.public_access_key", "");
  });

  it("keeps the credential input editable when saving fails", async () => {
    mockForm({
      sensitiveConfigured: ["s3.private_access_key"],
      dirtyCount: 1,
      save: vi.fn().mockRejectedValue(new Error("save failed")),
    });

    render(<InfrastructureSettings />);

    const privateGroup = within(screen.getByRole("group", { name: "Private storage" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(privateGroup.getByLabelText("Access Key")).toHaveAttribute("type", "password"),
    );
  });

  it("fails closed when protected credential status cannot be loaded", () => {
    mockForm({ sensitiveStatusReady: false, sensitiveStatusError: true });

    render(<InfrastructureSettings />);

    expect(screen.getByRole("alert")).toHaveTextContent(
      "Protected credential status is unavailable",
    );
    expect(screen.queryByLabelText("Access Key")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Secret Key")).not.toBeInTheDocument();
  });

  it("stages bucket override edits through the shared save model", async () => {
    const setValue = vi.fn();
    mockForm({
      setValue,
      isDirty: (key: string) => key === OPSLOG_BUCKET_POLICIES_KEY,
      dirtyCount: 1,
      getValue: (key: string) => {
        if (key === "s3.public_url_auth") return "presigned";
        if (key === OPSLOG_BUCKET_POLICIES_KEY) {
          return JSON.stringify([
            {
              component: "metadata",
              level: "info",
              retention_days: 1,
              max_rows: 100,
              max_size_mb: 8,
            },
          ]);
        }
        return "";
      },
    });

    render(<InfrastructureSettings />);

    await userEvent.click(screen.getByRole("button", { name: /Remove metadata rule/ }));

    expect(setValue).toHaveBeenCalledWith(OPSLOG_BUCKET_POLICIES_KEY, "[]");
  });

  describe("clearing a stored credential", () => {
    beforeEach(() => {
      realForm.enabled = true;
      serverSettings.current = { "s3.public_url_auth": "presigned" };
      sensitiveStatus.current = { configured: ["s3.public_access_key"], managed_by_env: [] };
      updateSettingsMock.mockReset();
      updateSettingsMock.mockResolvedValue({ values: {}, restart_required: false });
    });

    afterEach(() => {
      realForm.enabled = false;
    });

    it("stages the clear in the save bar and saves it as an empty value", async () => {
      render(<InfrastructureSettings />);

      const publicGroup = within(screen.getByRole("group", { name: "Public storage" }));
      await userEvent.click(publicGroup.getByRole("button", { name: "Clear saved value" }));

      expect(screen.getByText("1 unsaved change")).toBeInTheDocument();

      await userEvent.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() =>
        expect(updateSettingsMock).toHaveBeenCalledWith({ "s3.public_access_key": "" }),
      );
    });

    it("takes the clear back out of the save bar when the saved value is kept", async () => {
      render(<InfrastructureSettings />);

      const publicGroup = within(screen.getByRole("group", { name: "Public storage" }));
      await userEvent.click(publicGroup.getByRole("button", { name: "Clear saved value" }));
      await userEvent.click(publicGroup.getByRole("button", { name: "Keep saved value" }));

      expect(screen.queryByText("1 unsaved change")).not.toBeInTheDocument();
      expect(publicGroup.getByLabelText("Access Key")).toHaveAttribute(
        "placeholder",
        "••••••••••••",
      );
    });

    it("leaves an env-managed credential without a clear action", async () => {
      serverSettings.current = { "redis.url": "" };
      sensitiveStatus.current = {
        configured: ["redis.url"],
        managed_by_env: ["redis.url"],
      };

      render(<InfrastructureSettings />);

      const redisGroup = within(screen.getByRole("group", { name: "Redis" }));
      expect(redisGroup.getByLabelText("Connection URL")).toBeDisabled();
      expect(
        redisGroup.queryByRole("button", { name: "Clear saved value" }),
      ).not.toBeInTheDocument();
    });
  });
});
