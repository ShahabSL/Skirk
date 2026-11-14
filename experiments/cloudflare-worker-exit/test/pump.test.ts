import { describe, expect, it } from "vitest";
import { DriveApiError } from "../src/drive";
import { createInitialMetrics, pumpDriveChanges, type ChangeFeedClient, type PumpState } from "../src/pump";
import type { CandidateObject, DriveChangePage, SessionMetrics } from "../src/types";

class MemoryPumpState implements PumpState {
  pageToken?: string;
  metrics: SessionMetrics = createInitialMetrics(new Date("2026-05-24T00:00:00.000Z"));
  seen = new Set<string>();
  enableDedupe = false;

  async getPageToken(): Promise<string | undefined> {
    return this.pageToken;
  }

  async putPageToken(token: string): Promise<void> {
    this.pageToken = token;
  }

  async updateMetrics(update: (metrics: SessionMetrics) => SessionMetrics): Promise<SessionMetrics> {
    this.metrics = update(this.metrics);
    return this.metrics;
  }

  async dedupeCandidates(candidates: CandidateObject[]): Promise<CandidateObject[]> {
    if (!this.enableDedupe) {
      return candidates;
    }
    const fresh = [];
    for (const candidate of candidates) {
      if (this.seen.has(candidate.fileId)) {
        continue;
      }
      this.seen.add(candidate.fileId);
      fresh.push(candidate);
    }
    return fresh;
  }
}

describe("pumpDriveChanges", () => {
  it("initializes from start page token without replaying old Drive history", async () => {
    const state = new MemoryPumpState();
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "start-1",
      listChanges: async () => {
        throw new Error("listChanges should not run during initialization");
      },
    };

    const result = await pumpDriveChanges(drive, state, {
      prefix: "b20ff/",
      now: new Date("2026-05-24T00:01:00.000Z"),
    });

    expect(result.initialized).toBe(true);
    expect(result.pageToken).toBe("start-1");
    expect(state.pageToken).toBe("start-1");
    expect(state.metrics.initCount).toBe(1);
  });

  it("filters Skirk object names and advances to newStartPageToken", async () => {
    const state = new MemoryPumpState();
    state.pageToken = "page-1";
    const pages: Record<string, DriveChangePage> = {
      "page-1": {
        nextPageToken: "page-2",
        changes: [
          { fileId: "a", file: { id: "a", name: "b20ff/r1/p0/0001", size: "10", modifiedTime: "2026-05-24T01:00:00Z" } },
          { fileId: "b", file: { id: "b", name: "other/object", size: "20" } },
        ],
      },
      "page-2": {
        newStartPageToken: "start-2",
        changes: [
          { fileId: "c", removed: true },
          { fileId: "d", file: { id: "d", name: "b20ff/r1/p1/0002", size: "30" } },
        ],
      },
    };
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "unused",
      listChanges: async (pageToken) => pages[pageToken],
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "b20ff/" });

    expect(result).toMatchObject({
      initialized: false,
      pages: 2,
      changes: 4,
      matched: 2,
      removed: 1,
      truncated: false,
      pageToken: "start-2",
    });
    expect(result.candidates.map((candidate) => candidate.fileId)).toEqual(["a", "d"]);
    expect(state.pageToken).toBe("start-2");
    expect(state.metrics.totalMatched).toBe(2);
  });

  it("advances to nextPageToken when a bounded page is fully processed", async () => {
    const state = new MemoryPumpState();
    state.pageToken = "page-1";
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "unused",
      listChanges: async () => ({
        nextPageToken: "page-2",
        changes: [{ fileId: "a", file: { id: "a", name: "b20ff/object", size: "10" } }],
      }),
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "b20ff/", maxPages: 1 });

    expect(result.truncated).toBe(true);
    expect(result.pageToken).toBe("page-2");
    expect(state.pageToken).toBe("page-2");
  });

  it("keeps the old cursor when maxChanges stops in the middle of a page", async () => {
    const state = new MemoryPumpState();
    state.pageToken = "page-1";
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "unused",
      listChanges: async () => ({
        nextPageToken: "page-2",
        changes: [
          { fileId: "a", file: { id: "a", name: "b20ff/object-a", size: "10" } },
          { fileId: "b", file: { id: "b", name: "b20ff/object-b", size: "10" } },
        ],
      }),
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "b20ff/", maxChanges: 1 });

    expect(result.truncated).toBe(true);
    expect(result.pageToken).toBe("page-1");
    expect(state.pageToken).toBe("page-1");
  });

  it("resets the change cursor when Google rejects an old page token", async () => {
    const state = new MemoryPumpState();
    state.pageToken = "expired";
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "fresh",
      listChanges: async () => {
        throw new DriveApiError("gone", 410, "");
      },
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "" });

    expect(result.initialized).toBe(true);
    expect(result.pageToken).toBe("fresh");
    expect(state.pageToken).toBe("fresh");
    expect(state.metrics.resetCount).toBe(1);
  });

  it("deduplicates candidate metrics when the state supports it", async () => {
    const state = new MemoryPumpState();
    state.enableDedupe = true;
    state.pageToken = "page-1";
    state.seen.add("a");
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "unused",
      listChanges: async () => ({
        newStartPageToken: "start-2",
        changes: [
          { fileId: "a", file: { id: "a", name: "b20ff/object-a", size: "10" } },
          { fileId: "b", file: { id: "b", name: "b20ff/object-b", size: "10" } },
        ],
      }),
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "b20ff/" });

    expect(result.matched).toBe(1);
    expect(result.candidates.map((candidate) => candidate.fileId)).toEqual(["b"]);
    expect(state.metrics.totalMatched).toBe(1);
  });

  it("returns every matched candidate within the bounded change batch", async () => {
    const state = new MemoryPumpState();
    state.pageToken = "page-1";
    const changes = Array.from({ length: 75 }, (_, index) => ({
      fileId: `file-${index}`,
      file: { id: `file-${index}`, name: `b20ff/object-${index}`, size: "10" },
    }));
    const drive: ChangeFeedClient = {
      getStartPageToken: async () => "unused",
      listChanges: async () => ({
        newStartPageToken: "start-2",
        changes,
      }),
    };

    const result = await pumpDriveChanges(drive, state, { prefix: "b20ff/" });

    expect(result.matched).toBe(75);
    expect(result.candidates).toHaveLength(75);
    expect(result.candidates.at(-1)?.fileId).toBe("file-74");
  });
});
