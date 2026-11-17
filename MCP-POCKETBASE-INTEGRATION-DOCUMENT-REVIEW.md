# Review of the MCP–PocketBase integration document

Reviewed 2026-11-17. Scope: targeted source re-check of architectural claims and review against the explicitly proposed MCP 2025-06-18 baseline. Static review only; no runtime compatibility or OAuth flow was tested. This does not certify all inventory claims or establish support for newer protocol revisions.

Verdict: useful integration inventory; section 7 is not yet an implementation-ready design. Resolve the following findings before treating it as a package contract.

## 1. Accepted decision — reuse the full PocketBase session

Document location: [§7 AuthProvider](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:454), also §5.3.

The user explicitly accepts full PocketBase session reuse: a person authenticates as themselves, and MCP operates with that person's existing PB permissions. Access to ordinary PB endpoints with the same token is intentional. Narrower OAuth grants and MCP-specific credentials are not package requirements and do not block implementation.

Use the fork's normal PB token and the host's authorization path. Keep collection rules, application permissions, instance visibility, and any configured write gate authoritative. Do not claim that returned OAuth scopes restrict this token; describe any optional tool restrictions as application policy. No separate grant store or issuer changes are required for this accepted model.

## 2. Compatibility — define which remote clients support the accepted auth model

Document location: [§7 AuthProvider](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:454).

Full PB token reuse is the accepted authentication model. This resolves the product decision, but does not by itself establish compatibility with remote MCP clients that expect OAuth resource discovery and audience-bound tokens. Treat that as a client integration question rather than a reason to redesign credentials preemptively.

Start with explicit PB bearer-token connections and the fork's existing login flow where the selected client supports it. Document the supported clients and validate their discovery, registration and token behavior with a real connection. The MCP client performs the authorization-code flow; the MCP endpoint validates the resulting PB identity. Add metadata or other compatibility work when required by the selected clients. Do not claim full conformance to the MCP OAuth authorization profile unless its requirements are met.

Source: [MCP 2025-06-18 authorization](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/docs/specification/2025-06-18/basic/authorization.mdx). Registered-client PKCE is optional in the fork; static client registration and lack of refresh tokens are integration constraints, not reasons to reject the accepted model.

## 3. High — collection visibility needs a fork-specific instance boundary

Document location: [§7 ToolRegistry](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:447).

The proposed metadata fix considers collection ListRule/ViewRule only. The fork additionally has CollectionVisible, including instance scope and exclusions: [instance.go:113](/Volumes/mini512/Go%20Projects/pb-postgress/core/instance.go:113). Its normal token middleware checks auth-collection visibility at [middlewares.go:203](/Volumes/mini512/Go%20Projects/pb-postgress/apis/middlewares.go:203), and relation expansion checks related-collection visibility at [record_query_expand.go:161](/Volumes/mini512/Go%20Projects/pb-postgress/core/record_query_expand.go:161).

Correction: make instance visibility part of the adapter contract for discovery, descriptions, invocation, relations and cached schemas. Test scope changes after registry construction. This is a missing design requirement, not proof that current fork query APIs bypass visibility. Also spell out the original predicate precisely: hide for non-superusers when BOTH ListRule and ViewRule are nil. A non-nil conditional rule is not proof of access to every record.

## 4. Medium — the proposed search reuse contains a concrete SQL portability problem

Document location: [§7 DataAccess](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:463), §8 gap 4.

customjs explicitly uses CountCol("_rowid_") at [search.go:377](/Volumes/mini512/Go%20Projects/customjs/mcp/search.go:377). SQLite's implicit row identifier is not a portable PostgreSQL/MySQL column. Copying this implementation can break paginated search totals even when filters compile correctly.

Correction: use the selected host's supported count strategy and verify cardinality for relation joins. Add paginated-search checks on SQLite, PostgreSQL and MySQL if all three are promised. The document should name this known blocker rather than describe cross-dialect behavior only as generally unverified.

## 5. Medium — REST proxy capability is overstated by the shared registry design

Document location: [§7 ToolRegistry and DataAccess](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:447).

list_pocket demonstrates configured application endpoints, not discovery of arbitrary PB schemas using an ordinary user token. A sidecar cannot subscribe directly to in-process collection hooks. The proposed two modes therefore cannot automatically deliver the same dynamic schema-driven registry.

Correction: define backend capabilities explicitly. Support static endpoint manifests or configured schemas in REST mode, or specify a protected schema endpoint and refresh/invalidation contract. Keep privileged schema credentials out of ordinary user tool calls. Separate MCP core types from PB Record types so the REST adapter does not depend on embedded-host types.

## 6. Medium — the proposed spike cannot resolve the stated gaps

Document location: [§8 suggested first step](/Volumes/mini512/Components/Pocketbase-MCP/MCP-POCKETBASE-INTEGRATION-REVIEW.md:505).

Mounting a handler and calling initialize/tools/list can verify transport wiring and some compile compatibility. tools/list is a protocol method, not itself a data tool. This spike cannot establish collection metadata discovery, rule enforcement, dialect behavior, write hooks or OAuth interoperability, although the document says it resolves most of discovery gap 6.

Correction: separate compile/transport proof from behavioral proof. The minimum useful next spike includes a real collection-backed search/fetch tool; denied and permitted users; hidden and instance-excluded collections; pagination totals; and a schema change. Add a real authentication round trip with each selected remote client using the accepted full-session model. Test a real write and its request hook before promising write parity.

## Recommended revision

Keep the existing inventory and mark §7 as provisional. Define a PB-independent MCP core, version-specific embedded adapters, and a REST adapter with declared capabilities. Reuse the authenticated user's full PB session as explicitly accepted. Then prove one collection tool against a pinned stock release and a pinned fork revision before expanding tool generation or promising every deployment/backend combination.

The inventory's version and git-divergence claims are snapshots. Record inspected SHAs and working-tree status when revising; the historical SHAs alone do not identify uncommitted source content. No application code was changed by this review.
