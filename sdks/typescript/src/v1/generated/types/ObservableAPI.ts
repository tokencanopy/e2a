import { ResponseContext, RequestContext, HttpFile, HttpInfo } from '../http/http.js';
import { Configuration, ConfigurationOptions, mergeConfiguration } from '../configuration.js'
import type { Middleware } from '../middleware.js';
import { Observable, of, from } from '../rxjsStub.js';
import {mergeMap, map} from  '../rxjsStub.js';
import { APIKeyExportEntry } from '../models/APIKeyExportEntry.js';
import { APIKeyView } from '../models/APIKeyView.js';
import { AccountUserView } from '../models/AccountUserView.js';
import { AccountView } from '../models/AccountView.js';
import { AgentIdentity } from '../models/AgentIdentity.js';
import { AgentSuppressionAddedData } from '../models/AgentSuppressionAddedData.js';
import { AgentSuppressionView } from '../models/AgentSuppressionView.js';
import { AgentView } from '../models/AgentView.js';
import { ApproveRequest } from '../models/ApproveRequest.js';
import { Attachment } from '../models/Attachment.js';
import { AttachmentMetaView } from '../models/AttachmentMetaView.js';
import { AttachmentView } from '../models/AttachmentView.js';
import { ConversationDetailView } from '../models/ConversationDetailView.js';
import { ConversationSummaryView } from '../models/ConversationSummaryView.js';
import { CreateAPIKeyRequest } from '../models/CreateAPIKeyRequest.js';
import { CreateAPIKeyResponse } from '../models/CreateAPIKeyResponse.js';
import { CreateAgentRequest } from '../models/CreateAgentRequest.js';
import { CreateAgentSuppressionRequest } from '../models/CreateAgentSuppressionRequest.js';
import { CreateTemplateRequest } from '../models/CreateTemplateRequest.js';
import { CreateWebhookRequest } from '../models/CreateWebhookRequest.js';
import { CreateWebhookResponse } from '../models/CreateWebhookResponse.js';
import { DMARCResult } from '../models/DMARCResult.js';
import { DNSRecord } from '../models/DNSRecord.js';
import { DeleteAgentResult } from '../models/DeleteAgentResult.js';
import { DeleteApiKeyResult } from '../models/DeleteApiKeyResult.js';
import { DeleteDomainResult } from '../models/DeleteDomainResult.js';
import { DeleteMessageResult } from '../models/DeleteMessageResult.js';
import { DeleteSuppressionResult } from '../models/DeleteSuppressionResult.js';
import { DeleteTemplateResult } from '../models/DeleteTemplateResult.js';
import { DeleteUserDataResult } from '../models/DeleteUserDataResult.js';
import { DeleteWebhookResult } from '../models/DeleteWebhookResult.js';
import { DeliveryStatusJSON } from '../models/DeliveryStatusJSON.js';
import { DeploymentInfoView } from '../models/DeploymentInfoView.js';
import { Domain } from '../models/Domain.js';
import { DomainSendingFailedData } from '../models/DomainSendingFailedData.js';
import { DomainSendingVerifiedData } from '../models/DomainSendingVerifiedData.js';
import { DomainSuppressionAddedData } from '../models/DomainSuppressionAddedData.js';
import { DomainView } from '../models/DomainView.js';
import { EmailBouncedData } from '../models/EmailBouncedData.js';
import { EmailComplainedData } from '../models/EmailComplainedData.js';
import { EmailDeliveredData } from '../models/EmailDeliveredData.js';
import { EmailFailedData } from '../models/EmailFailedData.js';
import { EmailReceivedData } from '../models/EmailReceivedData.js';
import { EmailSentData } from '../models/EmailSentData.js';
import { ErrorBody } from '../models/ErrorBody.js';
import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { EventEnvelope } from '../models/EventEnvelope.js';
import { EventView } from '../models/EventView.js';
import { FieldError } from '../models/FieldError.js';
import { ForwardRequest } from '../models/ForwardRequest.js';
import { HoldReasonView } from '../models/HoldReasonView.js';
import { LimitExceededDetails } from '../models/LimitExceededDetails.js';
import { LimitExceededEnvelope } from '../models/LimitExceededEnvelope.js';
import { LimitExceededErrorBody } from '../models/LimitExceededErrorBody.js';
import { LimitsCapsView } from '../models/LimitsCapsView.js';
import { LimitsUsageView } from '../models/LimitsUsageView.js';
import { Message } from '../models/Message.js';
import { MessageBodyView } from '../models/MessageBodyView.js';
import { MessageLifecycleTransition } from '../models/MessageLifecycleTransition.js';
import { MessageParsedView } from '../models/MessageParsedView.js';
import { MessageSummaryView } from '../models/MessageSummaryView.js';
import { MessageView } from '../models/MessageView.js';
import { OAuthConnectionEntry } from '../models/OAuthConnectionEntry.js';
import { PageAPIKeyView } from '../models/PageAPIKeyView.js';
import { PageAgentSuppressionView } from '../models/PageAgentSuppressionView.js';
import { PageAgentView } from '../models/PageAgentView.js';
import { PageConversationSummaryView } from '../models/PageConversationSummaryView.js';
import { PageDomainView } from '../models/PageDomainView.js';
import { PageEventView } from '../models/PageEventView.js';
import { PageMessageLifecycleTransition } from '../models/PageMessageLifecycleTransition.js';
import { PageMessageSummaryView } from '../models/PageMessageSummaryView.js';
import { PageReviewView } from '../models/PageReviewView.js';
import { PageStarterTemplateView } from '../models/PageStarterTemplateView.js';
import { PageSuppressionView } from '../models/PageSuppressionView.js';
import { PageTemplateSummaryView } from '../models/PageTemplateSummaryView.js';
import { PageWebhookDeliveryView } from '../models/PageWebhookDeliveryView.js';
import { PageWebhookView } from '../models/PageWebhookView.js';
import { PayloadTooLargeDetails } from '../models/PayloadTooLargeDetails.js';
import { ProtectionConfigRequest } from '../models/ProtectionConfigRequest.js';
import { ProtectionConfigView } from '../models/ProtectionConfigView.js';
import { ProtectionDirectionRequest } from '../models/ProtectionDirectionRequest.js';
import { ProtectionDirectionView } from '../models/ProtectionDirectionView.js';
import { ProtectionEventExportEntry } from '../models/ProtectionEventExportEntry.js';
import { ProtectionFindingView } from '../models/ProtectionFindingView.js';
import { ProtectionGateRequest } from '../models/ProtectionGateRequest.js';
import { ProtectionGateView } from '../models/ProtectionGateView.js';
import { ProtectionHoldsRequest } from '../models/ProtectionHoldsRequest.js';
import { ProtectionHoldsView } from '../models/ProtectionHoldsView.js';
import { ProtectionScanRequest } from '../models/ProtectionScanRequest.js';
import { ProtectionScanView } from '../models/ProtectionScanView.js';
import { RateLimitedDetails } from '../models/RateLimitedDetails.js';
import { RateLimitedEnvelope } from '../models/RateLimitedEnvelope.js';
import { RateLimitedErrorBody } from '../models/RateLimitedErrorBody.js';
import { RedeliverDelivery } from '../models/RedeliverDelivery.js';
import { RedeliverEventRequest } from '../models/RedeliverEventRequest.js';
import { RedeliverView } from '../models/RedeliverView.js';
import { RegisterDomainRequest } from '../models/RegisterDomainRequest.js';
import { RejectRequest } from '../models/RejectRequest.js';
import { RejectResultView } from '../models/RejectResultView.js';
import { RenderedTemplateView } from '../models/RenderedTemplateView.js';
import { ReplyRequest } from '../models/ReplyRequest.js';
import { RetryAfterDetails } from '../models/RetryAfterDetails.js';
import { ReviewView } from '../models/ReviewView.js';
import { RotateSecretResponse } from '../models/RotateSecretResponse.js';
import { SPFResult } from '../models/SPFResult.js';
import { SendEmailRequest } from '../models/SendEmailRequest.js';
import { SendResultView } from '../models/SendResultView.js';
import { StarterTemplateDetailView } from '../models/StarterTemplateDetailView.js';
import { StarterTemplateVariableView } from '../models/StarterTemplateVariableView.js';
import { StarterTemplateView } from '../models/StarterTemplateView.js';
import { SuppressionExportEntry } from '../models/SuppressionExportEntry.js';
import { SuppressionView } from '../models/SuppressionView.js';
import { TemplatePartError } from '../models/TemplatePartError.js';
import { TemplateSummaryView } from '../models/TemplateSummaryView.js';
import { TemplateView } from '../models/TemplateView.js';
import { TestWebhookRequest } from '../models/TestWebhookRequest.js';
import { TestWebhookResponse } from '../models/TestWebhookResponse.js';
import { ThreatCategoryView } from '../models/ThreatCategoryView.js';
import { TooManyRecipientsDetails } from '../models/TooManyRecipientsDetails.js';
import { UnsubscribeOptions } from '../models/UnsubscribeOptions.js';
import { UpdateAgentRequest } from '../models/UpdateAgentRequest.js';
import { UpdateMessageRequest } from '../models/UpdateMessageRequest.js';
import { UpdateMessageResultView } from '../models/UpdateMessageResultView.js';
import { UpdateTemplateRequest } from '../models/UpdateTemplateRequest.js';
import { UpdateWebhookRequest } from '../models/UpdateWebhookRequest.js';
import { UsageEventEntry } from '../models/UsageEventEntry.js';
import { UserExport } from '../models/UserExport.js';
import { UserExportUser } from '../models/UserExportUser.js';
import { ValidateTemplateRequest } from '../models/ValidateTemplateRequest.js';
import { ValidateTemplateResponse } from '../models/ValidateTemplateResponse.js';
import { ValidationErrorDetails } from '../models/ValidationErrorDetails.js';
import { VerifyDomainView } from '../models/VerifyDomainView.js';
import { WebhookDeliveryView } from '../models/WebhookDeliveryView.js';
import { WebhookFiltersRequest } from '../models/WebhookFiltersRequest.js';
import { WebhookFiltersView } from '../models/WebhookFiltersView.js';
import { WebhookView } from '../models/WebhookView.js';

import { AccountApiRequestFactory, AccountApiResponseProcessor} from "../apis/AccountApi.js";
export class ObservableAccountApi {
    private requestFactory: AccountApiRequestFactory;
    private responseProcessor: AccountApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: AccountApiRequestFactory,
        responseProcessor?: AccountApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new AccountApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new AccountApiResponseProcessor();
    }

    /**
     * Mint a new API key; the plaintext key is returned once. scope=account is workspace admin (agent/domain/key management); scope=agent binds the key to one inbox so it can act only as that agent. Account scope only.
     * Create an API key
     * @param createAPIKeyRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME key — instead of minting a second live credential. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may mint a new key rather than replay.
     */
    public createApiKeyWithHttpInfo(createAPIKeyRequest: CreateAPIKeyRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<HttpInfo<CreateAPIKeyResponse>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.createApiKey(createAPIKeyRequest, idempotencyKey, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.createApiKeyWithHttpInfo(rsp)));
            }));
    }

    /**
     * Mint a new API key; the plaintext key is returned once. scope=account is workspace admin (agent/domain/key management); scope=agent binds the key to one inbox so it can act only as that agent. Account scope only.
     * Create an API key
     * @param createAPIKeyRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME key — instead of minting a second live credential. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may mint a new key rather than replay.
     */
    public createApiKey(createAPIKeyRequest: CreateAPIKeyRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<CreateAPIKeyResponse> {
        return this.createApiKeyWithHttpInfo(createAPIKeyRequest, idempotencyKey, _options).pipe(map((apiResponse: HttpInfo<CreateAPIKeyResponse>) => apiResponse.data));
    }

    /**
     * Permanently deletes the account and cascades all owned data. Requires ?confirm=DELETE. Returns 200 with a deletion receipt (deleted:true plus per-table cascade counts) — like every delete op, which all return 200 + a deletion object.
     * Delete your account + all data (irreversible)
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteAccountWithHttpInfo(confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteUserDataResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteAccount(confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteAccountWithHttpInfo(rsp)));
            }));
    }

    /**
     * Permanently deletes the account and cascades all owned data. Requires ?confirm=DELETE. Returns 200 with a deletion receipt (deleted:true plus per-table cascade counts) — like every delete op, which all return 200 + a deletion object.
     * Delete your account + all data (irreversible)
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteAccount(confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteUserDataResult> {
        return this.deleteAccountWithHttpInfo(confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteUserDataResult>) => apiResponse.data));
    }

    /**
     * Revoke a key by id. Integrations using it stop authenticating immediately. Account scope only. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Revoke an API key
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteApiKeyWithHttpInfo(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteApiKeyResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteApiKey(id, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteApiKeyWithHttpInfo(rsp)));
            }));
    }

    /**
     * Revoke a key by id. Integrations using it stop authenticating immediately. Account scope only. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Revoke an API key
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteApiKey(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteApiKeyResult> {
        return this.deleteApiKeyWithHttpInfo(id, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteApiKeyResult>) => apiResponse.data));
    }

    /**
     * Un-suppress a recipient. A previously-blocked send to it then succeeds (idempotency keys are released, so no fresh key is needed). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, address}).
     * Remove an address from the suppression list
     * @param address
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteSuppressionWithHttpInfo(address: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteSuppressionResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteSuppression(address, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteSuppressionWithHttpInfo(rsp)));
            }));
    }

    /**
     * Un-suppress a recipient. A previously-blocked send to it then succeeds (idempotency keys are released, so no fresh key is needed). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, address}).
     * Remove an address from the suppression list
     * @param address
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteSuppression(address: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteSuppressionResult> {
        return this.deleteSuppressionWithHttpInfo(address, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteSuppressionResult>) => apiResponse.data));
    }

    /**
     * A JSON dump of every record the authenticated account owns. Contract: the export envelope (the top-level keys and schema_version) is stable; the interior record shapes are versioned by schema_version and may evolve — branch on schema_version before interpreting interior records. Interior schemas carry `x-stability-level: beta` in this document to mark that exemption machine-readably; the operation itself is stable GA surface.
     * Export your data (GDPR right-of-access)
     */
    public exportAccountWithHttpInfo(_options?: ConfigurationOptions): Observable<HttpInfo<UserExport>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.exportAccount(_config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.exportAccountWithHttpInfo(rsp)));
            }));
    }

    /**
     * A JSON dump of every record the authenticated account owns. Contract: the export envelope (the top-level keys and schema_version) is stable; the interior record shapes are versioned by schema_version and may evolve — branch on schema_version before interpreting interior records. Interior schemas carry `x-stability-level: beta` in this document to mark that exemption machine-readably; the operation itself is stable GA surface.
     * Export your data (GDPR right-of-access)
     */
    public exportAccount(_options?: ConfigurationOptions): Observable<UserExport> {
        return this.exportAccountWithHttpInfo(_options).pipe(map((apiResponse: HttpInfo<UserExport>) => apiResponse.data));
    }

    /**
     * The authenticated principal\'s identity (user + scope; agent_email for agent-scoped credentials), plan caps, and current usage. Works for both account- and agent-scoped credentials. (Deployment discovery — shared domain, slug registration — is the separate public GET /v1/info.)
     * Get account: identity + plan limits + usage (whoami)
     */
    public getAccountWithHttpInfo(_options?: ConfigurationOptions): Observable<HttpInfo<AccountView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getAccount(_config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getAccountWithHttpInfo(rsp)));
            }));
    }

    /**
     * The authenticated principal\'s identity (user + scope; agent_email for agent-scoped credentials), plan caps, and current usage. Works for both account- and agent-scoped credentials. (Deployment discovery — shared domain, slug registration — is the separate public GET /v1/info.)
     * Get account: identity + plan limits + usage (whoami)
     */
    public getAccount(_options?: ConfigurationOptions): Observable<AccountView> {
        return this.getAccountWithHttpInfo(_options).pipe(map((apiResponse: HttpInfo<AccountView>) => apiResponse.data));
    }

    /**
     * API keys for the account (metadata only — secrets are shown once, at creation). Account scope only: an agent-scoped credential cannot manage keys.
     * List API keys
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listApiKeysWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageAPIKeyView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listApiKeys(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listApiKeysWithHttpInfo(rsp)));
            }));
    }

    /**
     * API keys for the account (metadata only — secrets are shown once, at creation). Account scope only: an agent-scoped credential cannot manage keys.
     * List API keys
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listApiKeys(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageAPIKeyView> {
        return this.listApiKeysWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageAPIKeyView>) => apiResponse.data));
    }

    /**
     * Addresses e2a will refuse to send to (auto-added on a hard bounce or complaint, or added manually). Sends to a suppressed address fail with recipient_suppressed.
     * List suppressed recipient addresses
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listSuppressionsWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageSuppressionView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listSuppressions(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listSuppressionsWithHttpInfo(rsp)));
            }));
    }

    /**
     * Addresses e2a will refuse to send to (auto-added on a hard bounce or complaint, or added manually). Sends to a suppressed address fail with recipient_suppressed.
     * List suppressed recipient addresses
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listSuppressions(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageSuppressionView> {
        return this.listSuppressionsWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageSuppressionView>) => apiResponse.data));
    }

}

import { AgentsApiRequestFactory, AgentsApiResponseProcessor} from "../apis/AgentsApi.js";
export class ObservableAgentsApi {
    private requestFactory: AgentsApiRequestFactory;
    private responseProcessor: AgentsApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: AgentsApiRequestFactory,
        responseProcessor?: AgentsApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new AgentsApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new AgentsApiResponseProcessor();
    }

    /**
     * Register an agent by full email. A custom-domain agent\'s domain must be a verified domain the caller owns; an email on the deployment\'s shared domain (e.g. xyz@agents.e2a.dev) is registered as a shared-domain agent. Returns the full agent.
     * Create an agent
     * @param createAgentRequest
     */
    public createAgentWithHttpInfo(createAgentRequest: CreateAgentRequest, _options?: ConfigurationOptions): Observable<HttpInfo<AgentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.createAgent(createAgentRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.createAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Register an agent by full email. A custom-domain agent\'s domain must be a verified domain the caller owns; an email on the deployment\'s shared domain (e.g. xyz@agents.e2a.dev) is registered as a shared-domain agent. Returns the full agent.
     * Create an agent
     * @param createAgentRequest
     */
    public createAgent(createAgentRequest: CreateAgentRequest, _options?: ConfigurationOptions): Observable<AgentView> {
        return this.createAgentWithHttpInfo(createAgentRequest, _options).pipe(map((apiResponse: HttpInfo<AgentView>) => apiResponse.data));
    }

    /**
     * Idempotently creates a manual recipient block for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Suppress a recipient for an agent (beta)
     * @param email
     * @param createAgentSuppressionRequest
     */
    public createAgentSuppressionWithHttpInfo(email: string, createAgentSuppressionRequest: CreateAgentSuppressionRequest, _options?: ConfigurationOptions): Observable<HttpInfo<AgentSuppressionView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.createAgentSuppression(email, createAgentSuppressionRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.createAgentSuppressionWithHttpInfo(rsp)));
            }));
    }

    /**
     * Idempotently creates a manual recipient block for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Suppress a recipient for an agent (beta)
     * @param email
     * @param createAgentSuppressionRequest
     */
    public createAgentSuppression(email: string, createAgentSuppressionRequest: CreateAgentSuppressionRequest, _options?: ConfigurationOptions): Observable<AgentSuppressionView> {
        return this.createAgentSuppressionWithHttpInfo(email, createAgentSuppressionRequest, _options).pipe(map((apiResponse: HttpInfo<AgentSuppressionView>) => apiResponse.data));
    }

    /**
     * Move an agent the caller owns to the trash. Requires ?confirm=DELETE. A trashed agent stops receiving mail, disappears from lists, and its held messages leave the review queue; restore it via POST /v1/agents/{email}/restore within the trash retention window — 30 days by default (deployment-configurable) — after which it is purged permanently (messages included). Live message data is otherwise retained indefinitely. Pass permanent=true to skip the trash and delete irreversibly right away (accepts live and trashed agents). Returns 200 with a deletion receipt; messages_deleted is zero when the agent is moved to trash.
     * Delete an agent
     * @param email
     * @param confirm Must be the literal DELETE. The default action moves the agent to trash; permanent&#x3D;true is irreversible.
     * @param [permanent] Delete irreversibly right away instead of moving to the trash. Accepts live and trashed agents.
     */
    public deleteAgentWithHttpInfo(email: string, confirm: 'DELETE', permanent?: boolean, _options?: ConfigurationOptions): Observable<HttpInfo<DeleteAgentResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteAgent(email, confirm, permanent, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Move an agent the caller owns to the trash. Requires ?confirm=DELETE. A trashed agent stops receiving mail, disappears from lists, and its held messages leave the review queue; restore it via POST /v1/agents/{email}/restore within the trash retention window — 30 days by default (deployment-configurable) — after which it is purged permanently (messages included). Live message data is otherwise retained indefinitely. Pass permanent=true to skip the trash and delete irreversibly right away (accepts live and trashed agents). Returns 200 with a deletion receipt; messages_deleted is zero when the agent is moved to trash.
     * Delete an agent
     * @param email
     * @param confirm Must be the literal DELETE. The default action moves the agent to trash; permanent&#x3D;true is irreversible.
     * @param [permanent] Delete irreversibly right away instead of moving to the trash. Accepts live and trashed agents.
     */
    public deleteAgent(email: string, confirm: 'DELETE', permanent?: boolean, _options?: ConfigurationOptions): Observable<DeleteAgentResult> {
        return this.deleteAgentWithHttpInfo(email, confirm, permanent, _options).pipe(map((apiResponse: HttpInfo<DeleteAgentResult>) => apiResponse.data));
    }

    /**
     * Removes only the exact agent-scoped block. Requires ?confirm=DELETE. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Remove an agent recipient suppression (beta)
     * @param email
     * @param address
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteAgentSuppressionWithHttpInfo(email: string, address: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteSuppressionResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteAgentSuppression(email, address, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteAgentSuppressionWithHttpInfo(rsp)));
            }));
    }

    /**
     * Removes only the exact agent-scoped block. Requires ?confirm=DELETE. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Remove an agent recipient suppression (beta)
     * @param email
     * @param address
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteAgentSuppression(email: string, address: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteSuppressionResult> {
        return this.deleteAgentSuppressionWithHttpInfo(email, address, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteSuppressionResult>) => apiResponse.data));
    }

    /**
     * Fetch a single agent the authenticated account owns, by full email address.
     * Get an agent
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public getAgentWithHttpInfo(email: string, _options?: ConfigurationOptions): Observable<HttpInfo<AgentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getAgent(email, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Fetch a single agent the authenticated account owns, by full email address.
     * Get an agent
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public getAgent(email: string, _options?: ConfigurationOptions): Observable<AgentView> {
        return this.getAgentWithHttpInfo(email, _options).pipe(map((apiResponse: HttpInfo<AgentView>) => apiResponse.data));
    }

    /**
     * Read the agent\'s protection posture — inbound/outbound trust gate, content-scan sensitivity, and hold-queue mechanism. Account scope only: an agent-scoped credential cannot read its own protection config. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Get an agent\'s protection config (beta)
     * @param email The agent\&#39;s full email address.
     */
    public getAgentProtectionWithHttpInfo(email: string, _options?: ConfigurationOptions): Observable<HttpInfo<ProtectionConfigView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getAgentProtection(email, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getAgentProtectionWithHttpInfo(rsp)));
            }));
    }

    /**
     * Read the agent\'s protection posture — inbound/outbound trust gate, content-scan sensitivity, and hold-queue mechanism. Account scope only: an agent-scoped credential cannot read its own protection config. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Get an agent\'s protection config (beta)
     * @param email The agent\&#39;s full email address.
     */
    public getAgentProtection(email: string, _options?: ConfigurationOptions): Observable<ProtectionConfigView> {
        return this.getAgentProtectionWithHttpInfo(email, _options).pipe(map((apiResponse: HttpInfo<ProtectionConfigView>) => apiResponse.data));
    }

    /**
     * Lists recipient addresses blocked only for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * List an agent\'s suppressed recipients (beta)
     * @param email
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listAgentSuppressionsWithHttpInfo(email: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageAgentSuppressionView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listAgentSuppressions(email, cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listAgentSuppressionsWithHttpInfo(rsp)));
            }));
    }

    /**
     * Lists recipient addresses blocked only for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * List an agent\'s suppressed recipients (beta)
     * @param email
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listAgentSuppressions(email: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageAgentSuppressionView> {
        return this.listAgentSuppressionsWithHttpInfo(email, cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageAgentSuppressionView>) => apiResponse.data));
    }

    /**
     * List the agents owned by the authenticated account, newest first, with cursor pagination. Pass deleted=true for the trash (soft-deleted agents, restorable until purged).
     * List agents
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     * @param [deleted] List the trash instead: agents that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live agents only).
     */
    public listAgentsWithHttpInfo(cursor?: string, limit?: number, deleted?: boolean, _options?: ConfigurationOptions): Observable<HttpInfo<PageAgentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listAgents(cursor, limit, deleted, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listAgentsWithHttpInfo(rsp)));
            }));
    }

    /**
     * List the agents owned by the authenticated account, newest first, with cursor pagination. Pass deleted=true for the trash (soft-deleted agents, restorable until purged).
     * List agents
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     * @param [deleted] List the trash instead: agents that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live agents only).
     */
    public listAgents(cursor?: string, limit?: number, deleted?: boolean, _options?: ConfigurationOptions): Observable<PageAgentView> {
        return this.listAgentsWithHttpInfo(cursor, limit, deleted, _options).pipe(map((apiResponse: HttpInfo<PageAgentView>) => apiResponse.data));
    }

    /**
     * Replace the agent\'s protection posture wholesale. The three top-level keys (inbound, outbound, holds) are required; leaves default. Account scope only. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Replace an agent\'s protection config (beta)
     * @param email The agent\&#39;s full email address.
     * @param protectionConfigRequest
     */
    public putAgentProtectionWithHttpInfo(email: string, protectionConfigRequest: ProtectionConfigRequest, _options?: ConfigurationOptions): Observable<HttpInfo<ProtectionConfigView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.putAgentProtection(email, protectionConfigRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.putAgentProtectionWithHttpInfo(rsp)));
            }));
    }

    /**
     * Replace the agent\'s protection posture wholesale. The three top-level keys (inbound, outbound, holds) are required; leaves default. Account scope only. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Replace an agent\'s protection config (beta)
     * @param email The agent\&#39;s full email address.
     * @param protectionConfigRequest
     */
    public putAgentProtection(email: string, protectionConfigRequest: ProtectionConfigRequest, _options?: ConfigurationOptions): Observable<ProtectionConfigView> {
        return this.putAgentProtectionWithHttpInfo(email, protectionConfigRequest, _options).pipe(map((apiResponse: HttpInfo<ProtectionConfigView>) => apiResponse.data));
    }

    /**
     * Bring a trashed (soft-deleted) agent back into service, messages and configuration intact. Live message retention is indefinite. For drafts still held for review, approval_expires_at is shifted forward by the time the agent spent in trash so a review hold cannot lapse while the inbox is unavailable. Returns the restored agent. 409 not_in_trash when the agent is not in the trash.
     * Restore an agent from the trash
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public restoreAgentWithHttpInfo(email: string, _options?: ConfigurationOptions): Observable<HttpInfo<AgentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.restoreAgent(email, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.restoreAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Bring a trashed (soft-deleted) agent back into service, messages and configuration intact. Live message retention is indefinite. For drafts still held for review, approval_expires_at is shifted forward by the time the agent spent in trash so a review hold cannot lapse while the inbox is unavailable. Returns the restored agent. 409 not_in_trash when the agent is not in the trash.
     * Restore an agent from the trash
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public restoreAgent(email: string, _options?: ConfigurationOptions): Observable<AgentView> {
        return this.restoreAgentWithHttpInfo(email, _options).pipe(map((apiResponse: HttpInfo<AgentView>) => apiResponse.data));
    }

    /**
     * Send a platform-originated test email (From: the platform noreply identity) to the agent\'s own address over the real external SMTP route, to confirm inbound delivery end to end. Returns 202: status=accepted (the message is durably persisted and queued; message_id is the GET-able e2a message id, and the terminal outcome arrives via GET /v1/messages/{id} or the email.sent / email.failed webhook events — provider_message_id appears only after provider submission) or status=pending_review when held for review. Always branch on body.status.
     * Send a test email to the agent\'s own address
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public testAgentWithHttpInfo(email: string, _options?: ConfigurationOptions): Observable<HttpInfo<SendResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.testAgent(email, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.testAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Send a platform-originated test email (From: the platform noreply identity) to the agent\'s own address over the real external SMTP route, to confirm inbound delivery end to end. Returns 202: status=accepted (the message is durably persisted and queued; message_id is the GET-able e2a message id, and the terminal outcome arrives via GET /v1/messages/{id} or the email.sent / email.failed webhook events — provider_message_id appears only after provider submission) or status=pending_review when held for review. Always branch on body.status.
     * Send a test email to the agent\'s own address
     * @param email The agent\&#39;s full email address, e.g. support@acme.com.
     */
    public testAgent(email: string, _options?: ConfigurationOptions): Observable<SendResultView> {
        return this.testAgentWithHttpInfo(email, _options).pipe(map((apiResponse: HttpInfo<SendResultView>) => apiResponse.data));
    }

    /**
     * Update an agent\'s display name. The screening/protection config lives on the /v1/agents/{email}/protection sub-resource. Returns the post-update agent.
     * Update an agent
     * @param email
     * @param updateAgentRequest
     */
    public updateAgentWithHttpInfo(email: string, updateAgentRequest: UpdateAgentRequest, _options?: ConfigurationOptions): Observable<HttpInfo<AgentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.updateAgent(email, updateAgentRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.updateAgentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Update an agent\'s display name. The screening/protection config lives on the /v1/agents/{email}/protection sub-resource. Returns the post-update agent.
     * Update an agent
     * @param email
     * @param updateAgentRequest
     */
    public updateAgent(email: string, updateAgentRequest: UpdateAgentRequest, _options?: ConfigurationOptions): Observable<AgentView> {
        return this.updateAgentWithHttpInfo(email, updateAgentRequest, _options).pipe(map((apiResponse: HttpInfo<AgentView>) => apiResponse.data));
    }

}

import { ConversationsApiRequestFactory, ConversationsApiResponseProcessor} from "../apis/ConversationsApi.js";
export class ObservableConversationsApi {
    private requestFactory: ConversationsApiRequestFactory;
    private responseProcessor: ConversationsApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: ConversationsApiRequestFactory,
        responseProcessor?: ConversationsApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new ConversationsApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new ConversationsApiResponseProcessor();
    }

    /**
     * Fetch a single conversation thread with its participants, labels, and member messages.
     * Get a conversation
     * @param email
     * @param id
     */
    public getConversationWithHttpInfo(email: string, id: string, _options?: ConfigurationOptions): Observable<HttpInfo<ConversationDetailView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getConversation(email, id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getConversationWithHttpInfo(rsp)));
            }));
    }

    /**
     * Fetch a single conversation thread with its participants, labels, and member messages.
     * Get a conversation
     * @param email
     * @param id
     */
    public getConversation(email: string, id: string, _options?: ConfigurationOptions): Observable<ConversationDetailView> {
        return this.getConversationWithHttpInfo(email, id, _options).pipe(map((apiResponse: HttpInfo<ConversationDetailView>) => apiResponse.data));
    }

    /**
     * List an agent\'s conversation threads (derived from messages.conversation_id).
     * List conversations
     * @param email
     * @param [since] RFC3339.
     * @param [until] RFC3339.
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change since/until.
     * @param [limit]
     */
    public listConversationsWithHttpInfo(email: string, since?: string, until?: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageConversationSummaryView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listConversations(email, since, until, cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listConversationsWithHttpInfo(rsp)));
            }));
    }

    /**
     * List an agent\'s conversation threads (derived from messages.conversation_id).
     * List conversations
     * @param email
     * @param [since] RFC3339.
     * @param [until] RFC3339.
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change since/until.
     * @param [limit]
     */
    public listConversations(email: string, since?: string, until?: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageConversationSummaryView> {
        return this.listConversationsWithHttpInfo(email, since, until, cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageConversationSummaryView>) => apiResponse.data));
    }

}

import { DomainsApiRequestFactory, DomainsApiResponseProcessor} from "../apis/DomainsApi.js";
export class ObservableDomainsApi {
    private requestFactory: DomainsApiRequestFactory;
    private responseProcessor: DomainsApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: DomainsApiRequestFactory,
        responseProcessor?: DomainsApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new DomainsApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new DomainsApiResponseProcessor();
    }

    /**
     * Deprovisions the domain\'s sending identity and breaks sending for every agent on it. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain}).
     * Delete a domain
     * @param domain
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteDomainWithHttpInfo(domain: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteDomainResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteDomain(domain, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteDomainWithHttpInfo(rsp)));
            }));
    }

    /**
     * Deprovisions the domain\'s sending identity and breaks sending for every agent on it. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain}).
     * Delete a domain
     * @param domain
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteDomain(domain: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteDomainResult> {
        return this.deleteDomainWithHttpInfo(domain, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteDomainResult>) => apiResponse.data));
    }

    /**
     * Get a domain
     * @param domain
     */
    public getDomainWithHttpInfo(domain: string, _options?: ConfigurationOptions): Observable<HttpInfo<DomainView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getDomain(domain, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getDomainWithHttpInfo(rsp)));
            }));
    }

    /**
     * Get a domain
     * @param domain
     */
    public getDomain(domain: string, _options?: ConfigurationOptions): Observable<DomainView> {
        return this.getDomainWithHttpInfo(domain, _options).pipe(map((apiResponse: HttpInfo<DomainView>) => apiResponse.data));
    }

    /**
     * List the domains owned by the authenticated account, newest first, with cursor pagination.
     * List domains
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listDomainsWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageDomainView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listDomains(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listDomainsWithHttpInfo(rsp)));
            }));
    }

    /**
     * List the domains owned by the authenticated account, newest first, with cursor pagination.
     * List domains
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listDomains(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageDomainView> {
        return this.listDomainsWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageDomainView>) => apiResponse.data));
    }

    /**
     * Register a domain
     * @param registerDomainRequest
     */
    public registerDomainWithHttpInfo(registerDomainRequest: RegisterDomainRequest, _options?: ConfigurationOptions): Observable<HttpInfo<DomainView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.registerDomain(registerDomainRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.registerDomainWithHttpInfo(rsp)));
            }));
    }

    /**
     * Register a domain
     * @param registerDomainRequest
     */
    public registerDomain(registerDomainRequest: RegisterDomainRequest, _options?: ConfigurationOptions): Observable<DomainView> {
        return this.registerDomainWithHttpInfo(registerDomainRequest, _options).pipe(map((apiResponse: HttpInfo<DomainView>) => apiResponse.data));
    }

    /**
     * Probe the domain\'s published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.
     * Verify a domain
     * @param domain
     */
    public verifyDomainWithHttpInfo(domain: string, _options?: ConfigurationOptions): Observable<HttpInfo<VerifyDomainView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.verifyDomain(domain, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.verifyDomainWithHttpInfo(rsp)));
            }));
    }

    /**
     * Probe the domain\'s published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.
     * Verify a domain
     * @param domain
     */
    public verifyDomain(domain: string, _options?: ConfigurationOptions): Observable<VerifyDomainView> {
        return this.verifyDomainWithHttpInfo(domain, _options).pipe(map((apiResponse: HttpInfo<VerifyDomainView>) => apiResponse.data));
    }

}

import { EventsApiRequestFactory, EventsApiResponseProcessor} from "../apis/EventsApi.js";
export class ObservableEventsApi {
    private requestFactory: EventsApiRequestFactory;
    private responseProcessor: EventsApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: EventsApiRequestFactory,
        responseProcessor?: EventsApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new EventsApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new EventsApiResponseProcessor();
    }

    /**
     * Get an event
     * @param id
     */
    public getEventWithHttpInfo(id: string, _options?: ConfigurationOptions): Observable<HttpInfo<EventView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getEvent(id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getEventWithHttpInfo(rsp)));
            }));
    }

    /**
     * Get an event
     * @param id
     */
    public getEvent(id: string, _options?: ConfigurationOptions): Observable<EventView> {
        return this.getEventWithHttpInfo(id, _options).pipe(map((apiResponse: HttpInfo<EventView>) => apiResponse.data));
    }

    /**
     * The webhook-event delivery log, filterable by type/agent/conversation/message and time range, with cursor pagination.
     * List events
     * @param [type]
     * @param [agentEmail]
     * @param [conversationId]
     * @param [messageId]
     * @param [since] RFC3339.
     * @param [until] RFC3339.
     * @param [cursor]
     * @param [limit]
     */
    public listEventsWithHttpInfo(type?: string, agentEmail?: string, conversationId?: string, messageId?: string, since?: string, until?: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageEventView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listEvents(type, agentEmail, conversationId, messageId, since, until, cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listEventsWithHttpInfo(rsp)));
            }));
    }

    /**
     * The webhook-event delivery log, filterable by type/agent/conversation/message and time range, with cursor pagination.
     * List events
     * @param [type]
     * @param [agentEmail]
     * @param [conversationId]
     * @param [messageId]
     * @param [since] RFC3339.
     * @param [until] RFC3339.
     * @param [cursor]
     * @param [limit]
     */
    public listEvents(type?: string, agentEmail?: string, conversationId?: string, messageId?: string, since?: string, until?: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageEventView> {
        return this.listEventsWithHttpInfo(type, agentEmail, conversationId, messageId, since, until, cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageEventView>) => apiResponse.data));
    }

    /**
     * Re-enqueue webhook delivery for an event. With a webhook_id, replays to that subscriber; without, fans out to every originally-matched subscriber. Auto-deduplicated within a short window — receivers must dedup on event id. Returns 202 Accepted: the redelivery is durably enqueued for async submission, not delivered synchronously — the per-subscriber outcome surfaces via the delivery log, and each delivery\'s status is \'pending\' (or \'scheduled\' for the fan-out).
     * Redeliver an event
     * @param id
     * @param redeliverEventRequest
     */
    public redeliverEventWithHttpInfo(id: string, redeliverEventRequest: RedeliverEventRequest, _options?: ConfigurationOptions): Observable<HttpInfo<RedeliverView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.redeliverEvent(id, redeliverEventRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.redeliverEventWithHttpInfo(rsp)));
            }));
    }

    /**
     * Re-enqueue webhook delivery for an event. With a webhook_id, replays to that subscriber; without, fans out to every originally-matched subscriber. Auto-deduplicated within a short window — receivers must dedup on event id. Returns 202 Accepted: the redelivery is durably enqueued for async submission, not delivered synchronously — the per-subscriber outcome surfaces via the delivery log, and each delivery\'s status is \'pending\' (or \'scheduled\' for the fan-out).
     * Redeliver an event
     * @param id
     * @param redeliverEventRequest
     */
    public redeliverEvent(id: string, redeliverEventRequest: RedeliverEventRequest, _options?: ConfigurationOptions): Observable<RedeliverView> {
        return this.redeliverEventWithHttpInfo(id, redeliverEventRequest, _options).pipe(map((apiResponse: HttpInfo<RedeliverView>) => apiResponse.data));
    }

}

import { MessagesApiRequestFactory, MessagesApiResponseProcessor} from "../apis/MessagesApi.js";
export class ObservableMessagesApi {
    private requestFactory: MessagesApiRequestFactory;
    private responseProcessor: MessagesApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: MessagesApiRequestFactory,
        responseProcessor?: MessagesApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new MessagesApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new MessagesApiResponseProcessor();
    }

    /**
     * Move a message to the trash. Trashed messages disappear from lists, threads, and reply targets, but can be restored via POST …/messages/{id}/restore until they are purged — 30 days after deletion by default (the trash retention window is deployment-configurable). Live message data is otherwise retained indefinitely. No confirmation is required because the default delete is reversible. Pass permanent=true with confirm=DELETE to permanently delete a message that is ALREADY in the trash (\"delete forever\"). A message held for review (review_status=pending_review) cannot be deleted — resolve it in the review queue first (409 message_held).
     * Delete a message (move to trash)
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     * @param [permanent] Permanently delete a message that is already in the trash (irreversible). Requires confirm&#x3D;DELETE and an account-scoped credential.
     * @param [confirm] Must be the literal string DELETE when permanent&#x3D;true; ignored otherwise.
     */
    public deleteMessageWithHttpInfo(email: string, id: string, permanent?: boolean, confirm?: string, _options?: ConfigurationOptions): Observable<HttpInfo<DeleteMessageResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteMessage(email, id, permanent, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Move a message to the trash. Trashed messages disappear from lists, threads, and reply targets, but can be restored via POST …/messages/{id}/restore until they are purged — 30 days after deletion by default (the trash retention window is deployment-configurable). Live message data is otherwise retained indefinitely. No confirmation is required because the default delete is reversible. Pass permanent=true with confirm=DELETE to permanently delete a message that is ALREADY in the trash (\"delete forever\"). A message held for review (review_status=pending_review) cannot be deleted — resolve it in the review queue first (409 message_held).
     * Delete a message (move to trash)
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     * @param [permanent] Permanently delete a message that is already in the trash (irreversible). Requires confirm&#x3D;DELETE and an account-scoped credential.
     * @param [confirm] Must be the literal string DELETE when permanent&#x3D;true; ignored otherwise.
     */
    public deleteMessage(email: string, id: string, permanent?: boolean, confirm?: string, _options?: ConfigurationOptions): Observable<DeleteMessageResult> {
        return this.deleteMessageWithHttpInfo(email, id, permanent, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteMessageResult>) => apiResponse.data));
    }

    /**
     * Forward a message (inbound or outbound) to new recipients; the original is quoted and its attachments are carried over by default. Any attachments[] you supply are added on top of the originals. 202 when held for HITL. Forwarding a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — a forward requires the source message to have actually been sent; retry once it is sent, or use wait=sent on the original send. Attachment limits apply to the combined set (carried-over originals + supplied): at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Forward a message
     * @param email
     * @param id
     * @param forwardRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public forwardMessageWithHttpInfo(email: string, id: string, forwardRequest: ForwardRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<HttpInfo<SendResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.forwardMessage(email, id, forwardRequest, idempotencyKey, wait, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.forwardMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Forward a message (inbound or outbound) to new recipients; the original is quoted and its attachments are carried over by default. Any attachments[] you supply are added on top of the originals. 202 when held for HITL. Forwarding a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — a forward requires the source message to have actually been sent; retry once it is sent, or use wait=sent on the original send. Attachment limits apply to the combined set (carried-over originals + supplied): at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Forward a message
     * @param email
     * @param id
     * @param forwardRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public forwardMessage(email: string, id: string, forwardRequest: ForwardRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<SendResultView> {
        return this.forwardMessageWithHttpInfo(email, id, forwardRequest, idempotencyKey, wait, _options).pipe(map((apiResponse: HttpInfo<SendResultView>) => apiResponse.data));
    }

    /**
     * Returns one attachment\'s metadata plus a short-lived `download_url` (+ `expires_at`) to fetch the bytes out of band — so binary content never streams through an agent\'s context. Pass `?inline=true` to also receive base64 `data` for small attachments (<= 256 KB); larger inline requests are rejected with 413 attachment_too_large. `index` is the 0-based attachment index from the message\'s `attachments[]`.
     * Get an attachment (metadata + short-lived download URL)
     * @param email
     * @param id
     * @param index
     * @param [inline] When true, also include the bytes as base64 in \&#39;data\&#39; — ONLY for attachments &lt;&#x3D; 256 KB; larger inline requests are rejected (413). Default false (use download_url).
     */
    public getAttachmentWithHttpInfo(email: string, id: string, index: number, inline?: boolean, _options?: ConfigurationOptions): Observable<HttpInfo<AttachmentView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getAttachment(email, id, index, inline, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getAttachmentWithHttpInfo(rsp)));
            }));
    }

    /**
     * Returns one attachment\'s metadata plus a short-lived `download_url` (+ `expires_at`) to fetch the bytes out of band — so binary content never streams through an agent\'s context. Pass `?inline=true` to also receive base64 `data` for small attachments (<= 256 KB); larger inline requests are rejected with 413 attachment_too_large. `index` is the 0-based attachment index from the message\'s `attachments[]`.
     * Get an attachment (metadata + short-lived download URL)
     * @param email
     * @param id
     * @param index
     * @param [inline] When true, also include the bytes as base64 in \&#39;data\&#39; — ONLY for attachments &lt;&#x3D; 256 KB; larger inline requests are rejected (413). Default false (use download_url).
     */
    public getAttachment(email: string, id: string, index: number, inline?: boolean, _options?: ConfigurationOptions): Observable<AttachmentView> {
        return this.getAttachmentWithHttpInfo(email, id, index, inline, _options).pipe(map((apiResponse: HttpInfo<AttachmentView>) => apiResponse.data));
    }

    /**
     * Fetch a single message (inbound or outbound) by id, scoped to an agent the caller owns. A trashed message remains readable by this direct GET and includes deleted_at until it is permanently purged (30 days after deletion by default, deployment-configurable); ordinary lists, conversations, reply targets, and forward targets exclude it. Includes the raw message and canonical inbound authentication evidence. Fetching an unread inbound message marks it read as a side effect.
     * Get a message
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     */
    public getMessageWithHttpInfo(email: string, id: string, _options?: ConfigurationOptions): Observable<HttpInfo<MessageView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getMessage(email, id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Fetch a single message (inbound or outbound) by id, scoped to an agent the caller owns. A trashed message remains readable by this direct GET and includes deleted_at until it is permanently purged (30 days after deletion by default, deployment-configurable); ordinary lists, conversations, reply targets, and forward targets exclude it. Includes the raw message and canonical inbound authentication evidence. Fetching an unread inbound message marks it read as a side effect.
     * Get a message
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     */
    public getMessage(email: string, id: string, _options?: ConfigurationOptions): Observable<MessageView> {
        return this.getMessageWithHttpInfo(email, id, _options).pipe(map((apiResponse: HttpInfo<MessageView>) => apiResponse.data));
    }

    /**
     * Returns the observations e2a recorded for one inbound or outbound message in deterministic ascending (occurred_at, id) order. Delivery means recipient-server acceptance and does not claim inbox placement. Beta: message lifecycle may change before it is declared stable.
     * Get a message\'s lifecycle (beta)
     * @param email
     * @param id
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor.
     * @param [limit] Maximum number of lifecycle transitions to return (1-100).
     */
    public getMessageLifecycleWithHttpInfo(email: string, id: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageMessageLifecycleTransition>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getMessageLifecycle(email, id, cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getMessageLifecycleWithHttpInfo(rsp)));
            }));
    }

    /**
     * Returns the observations e2a recorded for one inbound or outbound message in deterministic ascending (occurred_at, id) order. Delivery means recipient-server acceptance and does not claim inbox placement. Beta: message lifecycle may change before it is declared stable.
     * Get a message\'s lifecycle (beta)
     * @param email
     * @param id
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor.
     * @param [limit] Maximum number of lifecycle transitions to return (1-100).
     */
    public getMessageLifecycle(email: string, id: string, cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageMessageLifecycleTransition> {
        return this.getMessageLifecycleWithHttpInfo(email, id, cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageMessageLifecycleTransition>) => apiResponse.data));
    }

    /**
     * List an agent\'s messages (inbound + outbound) with filters and cursor pagination. Held outbound drafts appear as status=pending_review. Pass deleted=true for the trash (soft-deleted messages, restorable until purged — 30 days after deletion by default, deployment-configurable); the trash view defaults to direction=all and read_status=all.
     * List messages
     * @param email
     * @param [direction] Defaults to inbound.
     * @param [readStatus] Inbound only. Filters by inbox read-state (MSG-1). Defaults to unread for inbound, all otherwise.
     * @param [sort] Defaults to desc (newest first).
     * @param [from_] Case-insensitive substring match on sender.
     * @param [subjectContains] Case-insensitive substring match on subject.
     * @param [conversationId]
     * @param [labels] Comma-separated list (e.g. labels&#x3D;urgent,follow-up); AND-matched — a message must carry every given label.
     * @param [since] RFC3339; created_at &gt;&#x3D; since.
     * @param [until] RFC3339; created_at &lt; until.
     * @param [cursor]
     * @param [limit]
     * @param [deleted] List the trash instead: messages that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live messages only).
     */
    public listMessagesWithHttpInfo(email: string, direction?: 'inbound' | 'outbound' | 'all', readStatus?: 'unread' | 'read' | 'all', sort?: 'asc' | 'desc', from_?: string, subjectContains?: string, conversationId?: string, labels?: Array<string>, since?: string, until?: string, cursor?: string, limit?: number, deleted?: boolean, _options?: ConfigurationOptions): Observable<HttpInfo<PageMessageSummaryView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listMessages(email, direction, readStatus, sort, from_, subjectContains, conversationId, labels, since, until, cursor, limit, deleted, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listMessagesWithHttpInfo(rsp)));
            }));
    }

    /**
     * List an agent\'s messages (inbound + outbound) with filters and cursor pagination. Held outbound drafts appear as status=pending_review. Pass deleted=true for the trash (soft-deleted messages, restorable until purged — 30 days after deletion by default, deployment-configurable); the trash view defaults to direction=all and read_status=all.
     * List messages
     * @param email
     * @param [direction] Defaults to inbound.
     * @param [readStatus] Inbound only. Filters by inbox read-state (MSG-1). Defaults to unread for inbound, all otherwise.
     * @param [sort] Defaults to desc (newest first).
     * @param [from_] Case-insensitive substring match on sender.
     * @param [subjectContains] Case-insensitive substring match on subject.
     * @param [conversationId]
     * @param [labels] Comma-separated list (e.g. labels&#x3D;urgent,follow-up); AND-matched — a message must carry every given label.
     * @param [since] RFC3339; created_at &gt;&#x3D; since.
     * @param [until] RFC3339; created_at &lt; until.
     * @param [cursor]
     * @param [limit]
     * @param [deleted] List the trash instead: messages that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live messages only).
     */
    public listMessages(email: string, direction?: 'inbound' | 'outbound' | 'all', readStatus?: 'unread' | 'read' | 'all', sort?: 'asc' | 'desc', from_?: string, subjectContains?: string, conversationId?: string, labels?: Array<string>, since?: string, until?: string, cursor?: string, limit?: number, deleted?: boolean, _options?: ConfigurationOptions): Observable<PageMessageSummaryView> {
        return this.listMessagesWithHttpInfo(email, direction, readStatus, sort, from_, subjectContains, conversationId, labels, since, until, cursor, limit, deleted, _options).pipe(map((apiResponse: HttpInfo<PageMessageSummaryView>) => apiResponse.data));
    }

    /**
     * Reply to a message (inbound or outbound); recipients and threading are derived from the original. Replying to a message the agent received targets its sender; replying to a message the agent sent continues the thread to its original recipients (`reply_all` also re-includes the original Cc). 202 when held for HITL. Replying to a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — it has no Message-ID to thread onto; retry once it is sent, or use wait=sent on the original send. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Reply to a message
     * @param email
     * @param id
     * @param replyRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public replyToMessageWithHttpInfo(email: string, id: string, replyRequest: ReplyRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<HttpInfo<SendResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.replyToMessage(email, id, replyRequest, idempotencyKey, wait, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.replyToMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Reply to a message (inbound or outbound); recipients and threading are derived from the original. Replying to a message the agent received targets its sender; replying to a message the agent sent continues the thread to its original recipients (`reply_all` also re-includes the original Cc). 202 when held for HITL. Replying to a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — it has no Message-ID to thread onto; retry once it is sent, or use wait=sent on the original send. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Reply to a message
     * @param email
     * @param id
     * @param replyRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public replyToMessage(email: string, id: string, replyRequest: ReplyRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<SendResultView> {
        return this.replyToMessageWithHttpInfo(email, id, replyRequest, idempotencyKey, wait, _options).pipe(map((apiResponse: HttpInfo<SendResultView>) => apiResponse.data));
    }

    /**
     * Bring a trashed (soft-deleted) message back to the inbox. Restored message data is retained indefinitely unless it is deleted again. Returns the restored message. 409 not_in_trash when the message is not in the trash.
     * Restore a message from the trash
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     */
    public restoreMessageWithHttpInfo(email: string, id: string, _options?: ConfigurationOptions): Observable<HttpInfo<MessageView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.restoreMessage(email, id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.restoreMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Bring a trashed (soft-deleted) message back to the inbox. Restored message data is retained indefinitely unless it is deleted again. Returns the restored message. 409 not_in_trash when the message is not in the trash.
     * Restore a message from the trash
     * @param email The agent\&#39;s full email address.
     * @param id The message id, e.g. msg_abc123.
     */
    public restoreMessage(email: string, id: string, _options?: ConfigurationOptions): Observable<MessageView> {
        return this.restoreMessageWithHttpInfo(email, id, _options).pipe(map((apiResponse: HttpInfo<MessageView>) => apiResponse.data));
    }

    /**
     * Send a new email from the agent named in the path (a new thread). The sender is the path agent — `reply`/`forward` are their own sub-resources. 202 + pending_review when the agent has HITL enabled. Honors Idempotency-Key. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. Two capacity limits apply and are permanently distinct — branch on the HTTP status: 402 limit_exceeded is a QUOTA (monthly-message / storage stock-or-flow cap; a retry will not clear it — surface an upgrade path), 429 rate_limited is a throughput/request-RATE cap (transient; back off Retry-After seconds and retry).
     * Send a new email
     * @param email
     * @param sendEmailRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public sendMessageWithHttpInfo(email: string, sendEmailRequest: SendEmailRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<HttpInfo<SendResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.sendMessage(email, sendEmailRequest, idempotencyKey, wait, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.sendMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Send a new email from the agent named in the path (a new thread). The sender is the path agent — `reply`/`forward` are their own sub-resources. 202 + pending_review when the agent has HITL enabled. Honors Idempotency-Key. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. Two capacity limits apply and are permanently distinct — branch on the HTTP status: 402 limit_exceeded is a QUOTA (monthly-message / storage stock-or-flow cap; a retry will not clear it — surface an upgrade path), 429 rate_limited is a throughput/request-RATE cap (transient; back off Retry-After seconds and retry).
     * Send a new email
     * @param email
     * @param sendEmailRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * @param [wait] Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     */
    public sendMessage(email: string, sendEmailRequest: SendEmailRequest, idempotencyKey?: string, wait?: string, _options?: ConfigurationOptions): Observable<SendResultView> {
        return this.sendMessageWithHttpInfo(email, sendEmailRequest, idempotencyKey, wait, _options).pipe(map((apiResponse: HttpInfo<SendResultView>) => apiResponse.data));
    }

    /**
     * Apply a labels delta (`add_labels` / `remove_labels`) to a message the caller owns; returns the post-update label set. Each list is capped at 50 entries; labels are lowercase `[a-z0-9:_-]+` up to 64 chars; the `e2a:` prefix is reserved for system labels. A message carries at most 100 labels. An empty delta is a read of the current labels.
     * Update a message (labels)
     * @param email
     * @param id
     * @param updateMessageRequest
     */
    public updateMessageWithHttpInfo(email: string, id: string, updateMessageRequest: UpdateMessageRequest, _options?: ConfigurationOptions): Observable<HttpInfo<UpdateMessageResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.updateMessage(email, id, updateMessageRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.updateMessageWithHttpInfo(rsp)));
            }));
    }

    /**
     * Apply a labels delta (`add_labels` / `remove_labels`) to a message the caller owns; returns the post-update label set. Each list is capped at 50 entries; labels are lowercase `[a-z0-9:_-]+` up to 64 chars; the `e2a:` prefix is reserved for system labels. A message carries at most 100 labels. An empty delta is a read of the current labels.
     * Update a message (labels)
     * @param email
     * @param id
     * @param updateMessageRequest
     */
    public updateMessage(email: string, id: string, updateMessageRequest: UpdateMessageRequest, _options?: ConfigurationOptions): Observable<UpdateMessageResultView> {
        return this.updateMessageWithHttpInfo(email, id, updateMessageRequest, _options).pipe(map((apiResponse: HttpInfo<UpdateMessageResultView>) => apiResponse.data));
    }

}

import { MetaApiRequestFactory, MetaApiResponseProcessor} from "../apis/MetaApi.js";
export class ObservableMetaApi {
    private requestFactory: MetaApiRequestFactory;
    private responseProcessor: MetaApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: MetaApiRequestFactory,
        responseProcessor?: MetaApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new MetaApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new MetaApiResponseProcessor();
    }

    /**
     * Public deployment metadata: the shared agent domain (if slug registration is enabled) and the public base URL. Unauthenticated.
     * Deployment info
     */
    public getInfoWithHttpInfo(_options?: ConfigurationOptions): Observable<HttpInfo<DeploymentInfoView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getInfo(_config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getInfoWithHttpInfo(rsp)));
            }));
    }

    /**
     * Public deployment metadata: the shared agent domain (if slug registration is enabled) and the public base URL. Unauthenticated.
     * Deployment info
     */
    public getInfo(_options?: ConfigurationOptions): Observable<DeploymentInfoView> {
        return this.getInfoWithHttpInfo(_options).pipe(map((apiResponse: HttpInfo<DeploymentInfoView>) => apiResponse.data));
    }

}

import { ReviewsApiRequestFactory, ReviewsApiResponseProcessor} from "../apis/ReviewsApi.js";
export class ObservableReviewsApi {
    private requestFactory: ReviewsApiRequestFactory;
    private responseProcessor: ReviewsApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: ReviewsApiRequestFactory,
        responseProcessor?: ReviewsApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new ReviewsApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new ReviewsApiResponseProcessor();
    }

    /**
     * Approve a hold. Branches on direction: an outbound draft is durably queued for asynchronous delivery (honoring Idempotency-Key + optional reviewer overrides); an inbound hold is released to the inbox. Returns 202 with status=accepted for queued outbound delivery and 200 for an inbound release or local self-send loopback. Account-scoped only — an agent cannot approve its own hold. Approving an outbound draft applies the same per-agent send-rate limit as a direct send: 429 rate_limited when the agent is over its throughput limit (back off Retry-After seconds and retry). The merged outbound draft after applying reviewer overrides is subject to the same composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. The final merged recipient set (to, cc, and bcc, including reviewer overrides) is also re-checked against the account suppression list: any suppressed recipient returns 422 recipient_suppressed and the hold stays pending_review — remove the suppression (DELETE /v1/account/suppressions/{address}) and approve again. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Approve a held message (beta)
     * @param id
     * @param approveRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     */
    public approveReviewWithHttpInfo(id: string, approveRequest: ApproveRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<HttpInfo<SendResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.approveReview(id, approveRequest, idempotencyKey, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.approveReviewWithHttpInfo(rsp)));
            }));
    }

    /**
     * Approve a hold. Branches on direction: an outbound draft is durably queued for asynchronous delivery (honoring Idempotency-Key + optional reviewer overrides); an inbound hold is released to the inbox. Returns 202 with status=accepted for queued outbound delivery and 200 for an inbound release or local self-send loopback. Account-scoped only — an agent cannot approve its own hold. Approving an outbound draft applies the same per-agent send-rate limit as a direct send: 429 rate_limited when the agent is over its throughput limit (back off Retry-After seconds and retry). The merged outbound draft after applying reviewer overrides is subject to the same composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. The final merged recipient set (to, cc, and bcc, including reviewer overrides) is also re-checked against the account suppression list: any suppressed recipient returns 422 recipient_suppressed and the hold stays pending_review — remove the suppression (DELETE /v1/account/suppressions/{address}) and approve again. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Approve a held message (beta)
     * @param id
     * @param approveRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     */
    public approveReview(id: string, approveRequest: ApproveRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<SendResultView> {
        return this.approveReviewWithHttpInfo(id, approveRequest, idempotencyKey, _options).pipe(map((apiResponse: HttpInfo<SendResultView>) => apiResponse.data));
    }

    /**
     * Full detail of one held message — body + recipients (and, for inbound, the screening/auth context) — for a reviewer to make a decision. Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Get a held message (full detail, beta)
     * @param id
     */
    public getReviewWithHttpInfo(id: string, _options?: ConfigurationOptions): Observable<HttpInfo<MessageView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getReview(id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getReviewWithHttpInfo(rsp)));
            }));
    }

    /**
     * Full detail of one held message — body + recipients (and, for inbound, the screening/auth context) — for a reviewer to make a decision. Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Get a held message (full detail, beta)
     * @param id
     */
    public getReview(id: string, _options?: ConfigurationOptions): Observable<MessageView> {
        return this.getReviewWithHttpInfo(id, _options).pipe(map((apiResponse: HttpInfo<MessageView>) => apiResponse.data));
    }

    /**
     * The review queue: every message held in pending_review across the account\'s inboxes — outbound drafts awaiting send approval AND inbound messages held by a screening gate. Account-scoped credentials only; agents cannot see (or resolve) holds. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * List messages awaiting review (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listReviewsWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageReviewView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listReviews(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listReviewsWithHttpInfo(rsp)));
            }));
    }

    /**
     * The review queue: every message held in pending_review across the account\'s inboxes — outbound drafts awaiting send approval AND inbound messages held by a screening gate. Account-scoped credentials only; agents cannot see (or resolve) holds. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * List messages awaiting review (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listReviews(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageReviewView> {
        return this.listReviewsWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageReviewView>) => apiResponse.data));
    }

    /**
     * Reject a hold. An outbound draft is discarded (never sent); an inbound hold is dropped (never reaches the agent; payload retained hidden for forensics). Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Reject a held message (beta)
     * @param id
     * @param rejectRequest
     */
    public rejectReviewWithHttpInfo(id: string, rejectRequest: RejectRequest, _options?: ConfigurationOptions): Observable<HttpInfo<RejectResultView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.rejectReview(id, rejectRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.rejectReviewWithHttpInfo(rsp)));
            }));
    }

    /**
     * Reject a hold. An outbound draft is discarded (never sent); an inbound hold is dropped (never reaches the agent; payload retained hidden for forensics). Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Reject a held message (beta)
     * @param id
     * @param rejectRequest
     */
    public rejectReview(id: string, rejectRequest: RejectRequest, _options?: ConfigurationOptions): Observable<RejectResultView> {
        return this.rejectReviewWithHttpInfo(id, rejectRequest, _options).pipe(map((apiResponse: HttpInfo<RejectResultView>) => apiResponse.data));
    }

}

import { TemplatesApiRequestFactory, TemplatesApiResponseProcessor} from "../apis/TemplatesApi.js";
export class ObservableTemplatesApi {
    private requestFactory: TemplatesApiRequestFactory;
    private responseProcessor: TemplatesApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: TemplatesApiRequestFactory,
        responseProcessor?: TemplatesApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new TemplatesApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new TemplatesApiResponseProcessor();
    }

    /**
     * Create a reusable email template. subject and text (and html when present) must parse: {{variable}} interpolation with dot paths; {{{variable}}} renders raw in the HTML part. Alternatively set from_starter to copy a starter template verbatim. Beta: templates are unstable — their shape may change before they are declared stable.
     * Create a template (beta)
     * @param createTemplateRequest
     */
    public createTemplateWithHttpInfo(createTemplateRequest: CreateTemplateRequest, _options?: ConfigurationOptions): Observable<HttpInfo<TemplateView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.createTemplate(createTemplateRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.createTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Create a reusable email template. subject and text (and html when present) must parse: {{variable}} interpolation with dot paths; {{{variable}}} renders raw in the HTML part. Alternatively set from_starter to copy a starter template verbatim. Beta: templates are unstable — their shape may change before they are declared stable.
     * Create a template (beta)
     * @param createTemplateRequest
     */
    public createTemplate(createTemplateRequest: CreateTemplateRequest, _options?: ConfigurationOptions): Observable<TemplateView> {
        return this.createTemplateWithHttpInfo(createTemplateRequest, _options).pipe(map((apiResponse: HttpInfo<TemplateView>) => apiResponse.data));
    }

    /**
     * Delete a template. In-flight sends are unaffected (rendering happens at send time). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}). Beta: templates are unstable — their shape may change before they are declared stable.
     * Delete a template (beta)
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteTemplateWithHttpInfo(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteTemplateResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteTemplate(id, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Delete a template. In-flight sends are unaffected (rendering happens at send time). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}). Beta: templates are unstable — their shape may change before they are declared stable.
     * Delete a template (beta)
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteTemplate(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteTemplateResult> {
        return this.deleteTemplateWithHttpInfo(id, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteTemplateResult>) => apiResponse.data));
    }

    /**
     * Fetch one starter template by alias, including its full plain-text and HTML body sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a starter template (beta)
     * @param alias The starter template\&#39;s alias, e.g. welcome.
     */
    public getStarterTemplateWithHttpInfo(alias: string, _options?: ConfigurationOptions): Observable<HttpInfo<StarterTemplateDetailView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getStarterTemplate(alias, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getStarterTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Fetch one starter template by alias, including its full plain-text and HTML body sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a starter template (beta)
     * @param alias The starter template\&#39;s alias, e.g. welcome.
     */
    public getStarterTemplate(alias: string, _options?: ConfigurationOptions): Observable<StarterTemplateDetailView> {
        return this.getStarterTemplateWithHttpInfo(alias, _options).pipe(map((apiResponse: HttpInfo<StarterTemplateDetailView>) => apiResponse.data));
    }

    /**
     * Fetch one template by id. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a template (beta)
     * @param id
     */
    public getTemplateWithHttpInfo(id: string, _options?: ConfigurationOptions): Observable<HttpInfo<TemplateView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getTemplate(id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Fetch one template by id. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a template (beta)
     * @param id
     */
    public getTemplate(id: string, _options?: ConfigurationOptions): Observable<TemplateView> {
        return this.getTemplateWithHttpInfo(id, _options).pipe(map((apiResponse: HttpInfo<TemplateView>) => apiResponse.data));
    }

    /**
     * List the pre-built starter templates shipped with the deployment, sorted by alias. Returns catalog metadata only; fetch one by alias for the full body sources, or copy one into your library with from_starter on POST /v1/templates. Beta: templates are unstable — their shape may change before they are declared stable.
     * List starter templates (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listStarterTemplatesWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageStarterTemplateView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listStarterTemplates(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listStarterTemplatesWithHttpInfo(rsp)));
            }));
    }

    /**
     * List the pre-built starter templates shipped with the deployment, sorted by alias. Returns catalog metadata only; fetch one by alias for the full body sources, or copy one into your library with from_starter on POST /v1/templates. Beta: templates are unstable — their shape may change before they are declared stable.
     * List starter templates (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listStarterTemplates(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageStarterTemplateView> {
        return this.listStarterTemplatesWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageStarterTemplateView>) => apiResponse.data));
    }

    /**
     * List the account\'s templates, newest first. Returns metadata only (no text/html); fetch one by id for the full sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * List templates (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listTemplatesWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageTemplateSummaryView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listTemplates(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listTemplatesWithHttpInfo(rsp)));
            }));
    }

    /**
     * List the account\'s templates, newest first. Returns metadata only (no text/html); fetch one by id for the full sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * List templates (beta)
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listTemplates(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageTemplateSummaryView> {
        return this.listTemplatesWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageTemplateSummaryView>) => apiResponse.data));
    }

    /**
     * Partial update. Changed template parts are re-parsed; set alias or html to \"\" to clear them. Beta: templates are unstable — their shape may change before they are declared stable.
     * Update a template (beta)
     * @param id
     * @param updateTemplateRequest
     */
    public updateTemplateWithHttpInfo(id: string, updateTemplateRequest: UpdateTemplateRequest, _options?: ConfigurationOptions): Observable<HttpInfo<TemplateView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.updateTemplate(id, updateTemplateRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.updateTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Partial update. Changed template parts are re-parsed; set alias or html to \"\" to clear them. Beta: templates are unstable — their shape may change before they are declared stable.
     * Update a template (beta)
     * @param id
     * @param updateTemplateRequest
     */
    public updateTemplate(id: string, updateTemplateRequest: UpdateTemplateRequest, _options?: ConfigurationOptions): Observable<TemplateView> {
        return this.updateTemplateWithHttpInfo(id, updateTemplateRequest, _options).pipe(map((apiResponse: HttpInfo<TemplateView>) => apiResponse.data));
    }

    /**
     * Dry-run template source without persisting: reports per-part parse errors, a rendered preview (against test_data when provided), and suggested_data — a placeholder value for every variable the source references. Beta: templates are unstable — their shape may change before they are declared stable.
     * Validate template source (beta)
     * @param validateTemplateRequest
     */
    public validateTemplateWithHttpInfo(validateTemplateRequest: ValidateTemplateRequest, _options?: ConfigurationOptions): Observable<HttpInfo<ValidateTemplateResponse>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.validateTemplate(validateTemplateRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.validateTemplateWithHttpInfo(rsp)));
            }));
    }

    /**
     * Dry-run template source without persisting: reports per-part parse errors, a rendered preview (against test_data when provided), and suggested_data — a placeholder value for every variable the source references. Beta: templates are unstable — their shape may change before they are declared stable.
     * Validate template source (beta)
     * @param validateTemplateRequest
     */
    public validateTemplate(validateTemplateRequest: ValidateTemplateRequest, _options?: ConfigurationOptions): Observable<ValidateTemplateResponse> {
        return this.validateTemplateWithHttpInfo(validateTemplateRequest, _options).pipe(map((apiResponse: HttpInfo<ValidateTemplateResponse>) => apiResponse.data));
    }

}

import { WebhooksApiRequestFactory, WebhooksApiResponseProcessor} from "../apis/WebhooksApi.js";
export class ObservableWebhooksApi {
    private requestFactory: WebhooksApiRequestFactory;
    private responseProcessor: WebhooksApiResponseProcessor;
    private configuration: Configuration;

    public constructor(
        configuration: Configuration,
        requestFactory?: WebhooksApiRequestFactory,
        responseProcessor?: WebhooksApiResponseProcessor
    ) {
        this.configuration = configuration;
        this.requestFactory = requestFactory || new WebhooksApiRequestFactory(configuration);
        this.responseProcessor = responseProcessor || new WebhooksApiResponseProcessor();
    }

    /**
     * Register a webhook subscriber; the one-time signing secret is returned only on this response. Honors Idempotency-Key so a retried create replays the same webhook (same id + secret) instead of registering a second subscription; omit the key to intentionally create distinct subscriptions, including several to the same URL.
     * Create a webhook
     * @param createWebhookRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME webhook id and one-time signing secret — instead of registering a second active subscription. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). A keyed create commits the webhook and its replay response atomically, so an accepted create always replays; dedup is best-effort only under idempotency-store degradation before that commit.
     */
    public createWebhookWithHttpInfo(createWebhookRequest: CreateWebhookRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<HttpInfo<CreateWebhookResponse>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.createWebhook(createWebhookRequest, idempotencyKey, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.createWebhookWithHttpInfo(rsp)));
            }));
    }

    /**
     * Register a webhook subscriber; the one-time signing secret is returned only on this response. Honors Idempotency-Key so a retried create replays the same webhook (same id + secret) instead of registering a second subscription; omit the key to intentionally create distinct subscriptions, including several to the same URL.
     * Create a webhook
     * @param createWebhookRequest
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME webhook id and one-time signing secret — instead of registering a second active subscription. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). A keyed create commits the webhook and its replay response atomically, so an accepted create always replays; dedup is best-effort only under idempotency-store degradation before that commit.
     */
    public createWebhook(createWebhookRequest: CreateWebhookRequest, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<CreateWebhookResponse> {
        return this.createWebhookWithHttpInfo(createWebhookRequest, idempotencyKey, _options).pipe(map((apiResponse: HttpInfo<CreateWebhookResponse>) => apiResponse.data));
    }

    /**
     * Delete a webhook subscriber by id. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Delete a webhook
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteWebhookWithHttpInfo(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<HttpInfo<DeleteWebhookResult>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.deleteWebhook(id, confirm, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.deleteWebhookWithHttpInfo(rsp)));
            }));
    }

    /**
     * Delete a webhook subscriber by id. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Delete a webhook
     * @param id
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public deleteWebhook(id: string, confirm: 'DELETE', _options?: ConfigurationOptions): Observable<DeleteWebhookResult> {
        return this.deleteWebhookWithHttpInfo(id, confirm, _options).pipe(map((apiResponse: HttpInfo<DeleteWebhookResult>) => apiResponse.data));
    }

    /**
     * Get a webhook
     * @param id
     */
    public getWebhookWithHttpInfo(id: string, _options?: ConfigurationOptions): Observable<HttpInfo<WebhookView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.getWebhook(id, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.getWebhookWithHttpInfo(rsp)));
            }));
    }

    /**
     * Get a webhook
     * @param id
     */
    public getWebhook(id: string, _options?: ConfigurationOptions): Observable<WebhookView> {
        return this.getWebhookWithHttpInfo(id, _options).pipe(map((apiResponse: HttpInfo<WebhookView>) => apiResponse.data));
    }

    /**
     * The per-webhook delivery log (read-only debug view).
     * List webhook deliveries
     * @param id
     * @param [status]
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the status filter.
     * @param [limit]
     */
    public listWebhookDeliveriesWithHttpInfo(id: string, status?: 'pending' | 'delivered' | 'failed', cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageWebhookDeliveryView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listWebhookDeliveries(id, status, cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listWebhookDeliveriesWithHttpInfo(rsp)));
            }));
    }

    /**
     * The per-webhook delivery log (read-only debug view).
     * List webhook deliveries
     * @param id
     * @param [status]
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the status filter.
     * @param [limit]
     */
    public listWebhookDeliveries(id: string, status?: 'pending' | 'delivered' | 'failed', cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageWebhookDeliveryView> {
        return this.listWebhookDeliveriesWithHttpInfo(id, status, cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageWebhookDeliveryView>) => apiResponse.data));
    }

    /**
     * List the webhooks owned by the authenticated account, newest first, with cursor pagination.
     * List webhooks
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listWebhooksWithHttpInfo(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<HttpInfo<PageWebhookView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.listWebhooks(cursor, limit, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.listWebhooksWithHttpInfo(rsp)));
            }));
    }

    /**
     * List the webhooks owned by the authenticated account, newest first, with cursor pagination.
     * List webhooks
     * @param [cursor] Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param [limit] Maximum number of items to return (1-100).
     */
    public listWebhooks(cursor?: string, limit?: number, _options?: ConfigurationOptions): Observable<PageWebhookView> {
        return this.listWebhooksWithHttpInfo(cursor, limit, _options).pipe(map((apiResponse: HttpInfo<PageWebhookView>) => apiResponse.data));
    }

    /**
     * Mint a new signing secret; the previous one stays valid for a 24h grace window. Returns the new secret (shown once). Honors Idempotency-Key so a retried rotate replays the same secret instead of rotating twice (rotate has no request body, so the dedup hash covers the route alone — the same key on a different webhook id is a 422 idempotency_key_reuse).
     * Rotate a webhook signing secret
     * @param id
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     */
    public rotateWebhookSecretWithHttpInfo(id: string, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<HttpInfo<RotateSecretResponse>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.rotateWebhookSecret(id, idempotencyKey, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.rotateWebhookSecretWithHttpInfo(rsp)));
            }));
    }

    /**
     * Mint a new signing secret; the previous one stays valid for a 24h grace window. Returns the new secret (shown once). Honors Idempotency-Key so a retried rotate replays the same secret instead of rotating twice (rotate has no request body, so the dedup hash covers the route alone — the same key on a different webhook id is a 422 idempotency_key_reuse).
     * Rotate a webhook signing secret
     * @param id
     * @param [idempotencyKey] Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     */
    public rotateWebhookSecret(id: string, idempotencyKey?: string, _options?: ConfigurationOptions): Observable<RotateSecretResponse> {
        return this.rotateWebhookSecretWithHttpInfo(id, idempotencyKey, _options).pipe(map((apiResponse: HttpInfo<RotateSecretResponse>) => apiResponse.data));
    }

    /**
     * Schedule a one-off synthetic delivery to this webhook for development. Returns the delivery id.
     * Fire a synthetic event
     * @param id
     * @param testWebhookRequest
     */
    public testWebhookWithHttpInfo(id: string, testWebhookRequest: TestWebhookRequest, _options?: ConfigurationOptions): Observable<HttpInfo<TestWebhookResponse>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.testWebhook(id, testWebhookRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.testWebhookWithHttpInfo(rsp)));
            }));
    }

    /**
     * Schedule a one-off synthetic delivery to this webhook for development. Returns the delivery id.
     * Fire a synthetic event
     * @param id
     * @param testWebhookRequest
     */
    public testWebhook(id: string, testWebhookRequest: TestWebhookRequest, _options?: ConfigurationOptions): Observable<TestWebhookResponse> {
        return this.testWebhookWithHttpInfo(id, testWebhookRequest, _options).pipe(map((apiResponse: HttpInfo<TestWebhookResponse>) => apiResponse.data));
    }

    /**
     * Partial update. url/events/filters are full-replace when present. Re-enabling within the auto-disable cooldown returns 409.
     * Update a webhook
     * @param id
     * @param updateWebhookRequest
     */
    public updateWebhookWithHttpInfo(id: string, updateWebhookRequest: UpdateWebhookRequest, _options?: ConfigurationOptions): Observable<HttpInfo<WebhookView>> {
        const _config = mergeConfiguration(this.configuration, _options);

        const requestContextPromise = this.requestFactory.updateWebhook(id, updateWebhookRequest, _config);
        // build promise chain
        let middlewarePreObservable = from<RequestContext>(requestContextPromise);
        for (const middleware of _config.middleware) {
            middlewarePreObservable = middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => middleware.pre(ctx)));
        }

        return middlewarePreObservable.pipe(mergeMap((ctx: RequestContext) => _config.httpApi.send(ctx))).
            pipe(mergeMap((response: ResponseContext) => {
                let middlewarePostObservable = of(response);
                for (const middleware of _config.middleware.reverse()) {
                    middlewarePostObservable = middlewarePostObservable.pipe(mergeMap((rsp: ResponseContext) => middleware.post(rsp)));
                }
                return middlewarePostObservable.pipe(map((rsp: ResponseContext) => this.responseProcessor.updateWebhookWithHttpInfo(rsp)));
            }));
    }

    /**
     * Partial update. url/events/filters are full-replace when present. Re-enabling within the auto-disable cooldown returns 409.
     * Update a webhook
     * @param id
     * @param updateWebhookRequest
     */
    public updateWebhook(id: string, updateWebhookRequest: UpdateWebhookRequest, _options?: ConfigurationOptions): Observable<WebhookView> {
        return this.updateWebhookWithHttpInfo(id, updateWebhookRequest, _options).pipe(map((apiResponse: HttpInfo<WebhookView>) => apiResponse.data));
    }

}
