import { describe, expect, it } from "vitest";
import { duration, mib, percent, label, nodeStatus } from "./format.js";

describe("format", () => {
  it("renders durations and sizes", () => {
    expect(duration(90061)).toBe("1天 01:01:01");
    expect(duration(59)).toBe("00:00:59");
    expect(duration(null)).toBe("—");
    expect(mib(512)).toBe("512 MiB");
    expect(mib(2048)).toBe("2.0 GiB");
    expect(percent(1, 4)).toBe(25);
    expect(percent(1, 0)).toBe(0);
    expect(label(nodeStatus, "drain")).toBe("排空");
    expect(label(nodeStatus, "weird")).toBe("weird");
  });
});

import { overall } from "./currentstatus.js";
describe("overall cluster status", () => {
  it("matches the server's classification", () => {
    expect(overall({})).toBe("unavailable");
    expect(overall({ offline: 3 })).toBe("offline");
    expect(overall({ idle: 2, offline: 1 })).toBe("abnormal");
    expect(overall({ idle: 2, drain: 1 })).toBe("partial_maintenance");
    expect(overall({ idle: 2, running: 1 })).toBe("normal");
  });
});
