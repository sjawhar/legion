import { describe, expect, it } from "bun:test";
import {
  HealthCheckResponseSchema,
  LinearTeamsResponseSchema,
  SessionCreateResponseSchema,
} from "../schemas";

describe("schemas", () => {
  describe("LinearTeamsResponseSchema", () => {
    it("validates a successful Linear teams response", () => {
      const validResponse = {
        data: {
          teams: {
            nodes: [
              {
                id: "team-1",
                key: "ENG",
                name: "Engineering",
              },
              {
                id: "team-2",
                key: "DES",
                name: "Design",
              },
            ],
          },
        },
      };

      const result = LinearTeamsResponseSchema.safeParse(validResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.data?.teams.nodes).toHaveLength(2);
        expect(result.data.data?.teams.nodes[0].key).toBe("ENG");
      }
    });

    it("validates a Linear error response with null data", () => {
      const errorResponse = {
        data: null,
        errors: [
          {
            message: "Authentication required",
          },
        ],
      };

      const result = LinearTeamsResponseSchema.safeParse(errorResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.data).toBeNull();
        expect(result.data.errors).toHaveLength(1);
        expect(result.data.errors?.[0].message).toBe("Authentication required");
      }
    });

    it("accepts response with data field omitted (GraphQL error with no data)", () => {
      const errorResponse = {
        errors: [{ message: "auth error" }],
      };

      const result = LinearTeamsResponseSchema.safeParse(errorResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.data).toBeUndefined();
        expect(result.data.errors?.[0].message).toBe("auth error");
      }
    });

    it("allows extra fields via passthrough", () => {
      const responseWithExtra = {
        data: {
          teams: {
            nodes: [
              {
                id: "team-1",
                key: "ENG",
                name: "Engineering",
                extraField: "should be allowed",
              },
            ],
          },
          extraDataField: "also allowed",
        },
        extraTopLevel: "allowed too",
      };

      const result = LinearTeamsResponseSchema.safeParse(responseWithExtra);
      expect(result.success).toBe(true);
    });

    it("allows response with only data field (no errors)", () => {
      const responseNoErrors = {
        data: {
          teams: {
            nodes: [
              {
                id: "team-1",
                key: "ENG",
                name: "Engineering",
              },
            ],
          },
        },
      };

      const result = LinearTeamsResponseSchema.safeParse(responseNoErrors);
      expect(result.success).toBe(true);
    });
  });

  describe("SessionCreateResponseSchema", () => {
    it("validates a successful session creation response", () => {
      const validResponse = {
        id: "ses_abc123def456",
      };

      const result = SessionCreateResponseSchema.safeParse(validResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.id).toBe("ses_abc123def456");
      }
    });

    it("rejects response missing id field", () => {
      const invalidResponse = {};

      const result = SessionCreateResponseSchema.safeParse(invalidResponse);
      expect(result.success).toBe(false);
    });

    it("rejects response with non-string id", () => {
      const invalidResponse = {
        id: 12345,
      };

      const result = SessionCreateResponseSchema.safeParse(invalidResponse);
      expect(result.success).toBe(false);
    });

    it("allows extra fields via passthrough", () => {
      const responseWithExtra = {
        id: "ses_abc123def456",
        port: 13381,
        extraField: "allowed",
      };

      const result = SessionCreateResponseSchema.safeParse(responseWithExtra);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.id).toBe("ses_abc123def456");
      }
    });
  });

  describe("HealthCheckResponseSchema", () => {
    it("validates a healthy response", () => {
      const validResponse = {
        healthy: true,
      };

      const result = HealthCheckResponseSchema.safeParse(validResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.healthy).toBe(true);
      }
    });

    it("validates an unhealthy response", () => {
      const validResponse = {
        healthy: false,
      };

      const result = HealthCheckResponseSchema.safeParse(validResponse);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.healthy).toBe(false);
      }
    });

    it("rejects response missing healthy field", () => {
      const invalidResponse = {};

      const result = HealthCheckResponseSchema.safeParse(invalidResponse);
      expect(result.success).toBe(false);
    });

    it("rejects response with non-boolean healthy", () => {
      const invalidResponse = {
        healthy: "yes",
      };

      const result = HealthCheckResponseSchema.safeParse(invalidResponse);
      expect(result.success).toBe(false);
    });

    it("allows extra fields via passthrough", () => {
      const responseWithExtra = {
        healthy: true,
        uptime: 3600,
        workerCount: 5,
        extraField: "allowed",
      };

      const result = HealthCheckResponseSchema.safeParse(responseWithExtra);
      expect(result.success).toBe(true);
      if (result.success) {
        expect(result.data.healthy).toBe(true);
      }
    });
  });
});
