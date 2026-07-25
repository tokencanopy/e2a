import { ResponseContext, RequestContext, HttpFile, HttpInfo } from '../http/http.js';
import { Configuration, ConfigurationOptions } from '../configuration.js'
import type { Middleware } from '../middleware.js';

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

import { ObservableAccountApi } from "./ObservableAPI.js";
import { AccountApiRequestFactory, AccountApiResponseProcessor} from "../apis/AccountApi.js";

export interface AccountApiCreateApiKeyRequest {
    /**
     *
     * @type CreateAPIKeyRequest
     * @memberof AccountApicreateApiKey
     */
    createAPIKeyRequest: CreateAPIKeyRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME key — instead of minting a second live credential. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may mint a new key rather than replay.
     * Defaults to: undefined
     * @type string
     * @memberof AccountApicreateApiKey
     */
    idempotencyKey?: string
}

export interface AccountApiDeleteAccountRequest {
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof AccountApideleteAccount
     */
    confirm: 'DELETE'
}

export interface AccountApiDeleteApiKeyRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AccountApideleteApiKey
     */
    id: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof AccountApideleteApiKey
     */
    confirm: 'DELETE'
}

export interface AccountApiDeleteSuppressionRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AccountApideleteSuppression
     */
    address: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof AccountApideleteSuppression
     */
    confirm: 'DELETE'
}

export interface AccountApiExportAccountRequest {
}

export interface AccountApiGetAccountRequest {
}

export interface AccountApiListApiKeysRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof AccountApilistApiKeys
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof AccountApilistApiKeys
     */
    limit?: number
}

export interface AccountApiListSuppressionsRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof AccountApilistSuppressions
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof AccountApilistSuppressions
     */
    limit?: number
}

export class ObjectAccountApi {
    private api: ObservableAccountApi

    public constructor(configuration: Configuration, requestFactory?: AccountApiRequestFactory, responseProcessor?: AccountApiResponseProcessor) {
        this.api = new ObservableAccountApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Mint a new API key; the plaintext key is returned once. scope=account is workspace admin (agent/domain/key management); scope=agent binds the key to one inbox so it can act only as that agent. Account scope only.
     * Create an API key
     * @param param the request object
     */
    public createApiKeyWithHttpInfo(param: AccountApiCreateApiKeyRequest, options?: ConfigurationOptions): Promise<HttpInfo<CreateAPIKeyResponse>> {
        return this.api.createApiKeyWithHttpInfo(param.createAPIKeyRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Mint a new API key; the plaintext key is returned once. scope=account is workspace admin (agent/domain/key management); scope=agent binds the key to one inbox so it can act only as that agent. Account scope only.
     * Create an API key
     * @param param the request object
     */
    public createApiKey(param: AccountApiCreateApiKeyRequest, options?: ConfigurationOptions): Promise<CreateAPIKeyResponse> {
        return this.api.createApiKey(param.createAPIKeyRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Permanently deletes the account and cascades all owned data. Requires ?confirm=DELETE. Returns 200 with a deletion receipt (deleted:true plus per-table cascade counts) — like every delete op, which all return 200 + a deletion object.
     * Delete your account + all data (irreversible)
     * @param param the request object
     */
    public deleteAccountWithHttpInfo(param: AccountApiDeleteAccountRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteUserDataResult>> {
        return this.api.deleteAccountWithHttpInfo(param.confirm,  options).toPromise();
    }

    /**
     * Permanently deletes the account and cascades all owned data. Requires ?confirm=DELETE. Returns 200 with a deletion receipt (deleted:true plus per-table cascade counts) — like every delete op, which all return 200 + a deletion object.
     * Delete your account + all data (irreversible)
     * @param param the request object
     */
    public deleteAccount(param: AccountApiDeleteAccountRequest, options?: ConfigurationOptions): Promise<DeleteUserDataResult> {
        return this.api.deleteAccount(param.confirm,  options).toPromise();
    }

    /**
     * Revoke a key by id. Integrations using it stop authenticating immediately. Account scope only. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Revoke an API key
     * @param param the request object
     */
    public deleteApiKeyWithHttpInfo(param: AccountApiDeleteApiKeyRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteApiKeyResult>> {
        return this.api.deleteApiKeyWithHttpInfo(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Revoke a key by id. Integrations using it stop authenticating immediately. Account scope only. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Revoke an API key
     * @param param the request object
     */
    public deleteApiKey(param: AccountApiDeleteApiKeyRequest, options?: ConfigurationOptions): Promise<DeleteApiKeyResult> {
        return this.api.deleteApiKey(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Un-suppress a recipient. A previously-blocked send to it then succeeds (idempotency keys are released, so no fresh key is needed). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, address}).
     * Remove an address from the suppression list
     * @param param the request object
     */
    public deleteSuppressionWithHttpInfo(param: AccountApiDeleteSuppressionRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteSuppressionResult>> {
        return this.api.deleteSuppressionWithHttpInfo(param.address, param.confirm,  options).toPromise();
    }

    /**
     * Un-suppress a recipient. A previously-blocked send to it then succeeds (idempotency keys are released, so no fresh key is needed). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, address}).
     * Remove an address from the suppression list
     * @param param the request object
     */
    public deleteSuppression(param: AccountApiDeleteSuppressionRequest, options?: ConfigurationOptions): Promise<DeleteSuppressionResult> {
        return this.api.deleteSuppression(param.address, param.confirm,  options).toPromise();
    }

    /**
     * A JSON dump of every record the authenticated account owns. Contract: the export envelope (the top-level keys and schema_version) is stable; the interior record shapes are versioned by schema_version and may evolve — branch on schema_version before interpreting interior records. Interior schemas carry `x-stability-level: beta` in this document to mark that exemption machine-readably; the operation itself is stable GA surface.
     * Export your data (GDPR right-of-access)
     * @param param the request object
     */
    public exportAccountWithHttpInfo(param: AccountApiExportAccountRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<UserExport>> {
        return this.api.exportAccountWithHttpInfo( options).toPromise();
    }

    /**
     * A JSON dump of every record the authenticated account owns. Contract: the export envelope (the top-level keys and schema_version) is stable; the interior record shapes are versioned by schema_version and may evolve — branch on schema_version before interpreting interior records. Interior schemas carry `x-stability-level: beta` in this document to mark that exemption machine-readably; the operation itself is stable GA surface.
     * Export your data (GDPR right-of-access)
     * @param param the request object
     */
    public exportAccount(param: AccountApiExportAccountRequest = {}, options?: ConfigurationOptions): Promise<UserExport> {
        return this.api.exportAccount( options).toPromise();
    }

    /**
     * The authenticated principal\'s identity (user + scope; agent_email for agent-scoped credentials), plan caps, and current usage. Works for both account- and agent-scoped credentials. (Deployment discovery — shared domain, slug registration — is the separate public GET /v1/info.)
     * Get account: identity + plan limits + usage (whoami)
     * @param param the request object
     */
    public getAccountWithHttpInfo(param: AccountApiGetAccountRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<AccountView>> {
        return this.api.getAccountWithHttpInfo( options).toPromise();
    }

    /**
     * The authenticated principal\'s identity (user + scope; agent_email for agent-scoped credentials), plan caps, and current usage. Works for both account- and agent-scoped credentials. (Deployment discovery — shared domain, slug registration — is the separate public GET /v1/info.)
     * Get account: identity + plan limits + usage (whoami)
     * @param param the request object
     */
    public getAccount(param: AccountApiGetAccountRequest = {}, options?: ConfigurationOptions): Promise<AccountView> {
        return this.api.getAccount( options).toPromise();
    }

    /**
     * API keys for the account (metadata only — secrets are shown once, at creation). Account scope only: an agent-scoped credential cannot manage keys.
     * List API keys
     * @param param the request object
     */
    public listApiKeysWithHttpInfo(param: AccountApiListApiKeysRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageAPIKeyView>> {
        return this.api.listApiKeysWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * API keys for the account (metadata only — secrets are shown once, at creation). Account scope only: an agent-scoped credential cannot manage keys.
     * List API keys
     * @param param the request object
     */
    public listApiKeys(param: AccountApiListApiKeysRequest = {}, options?: ConfigurationOptions): Promise<PageAPIKeyView> {
        return this.api.listApiKeys(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Addresses e2a will refuse to send to (auto-added on a hard bounce or complaint, or added manually). Sends to a suppressed address fail with recipient_suppressed.
     * List suppressed recipient addresses
     * @param param the request object
     */
    public listSuppressionsWithHttpInfo(param: AccountApiListSuppressionsRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageSuppressionView>> {
        return this.api.listSuppressionsWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Addresses e2a will refuse to send to (auto-added on a hard bounce or complaint, or added manually). Sends to a suppressed address fail with recipient_suppressed.
     * List suppressed recipient addresses
     * @param param the request object
     */
    public listSuppressions(param: AccountApiListSuppressionsRequest = {}, options?: ConfigurationOptions): Promise<PageSuppressionView> {
        return this.api.listSuppressions(param.cursor, param.limit,  options).toPromise();
    }

}

import { ObservableAgentsApi } from "./ObservableAPI.js";
import { AgentsApiRequestFactory, AgentsApiResponseProcessor} from "../apis/AgentsApi.js";

export interface AgentsApiCreateAgentRequest {
    /**
     *
     * @type CreateAgentRequest
     * @memberof AgentsApicreateAgent
     */
    createAgentRequest: CreateAgentRequest
}

export interface AgentsApiCreateAgentSuppressionRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApicreateAgentSuppression
     */
    email: string
    /**
     *
     * @type CreateAgentSuppressionRequest
     * @memberof AgentsApicreateAgentSuppression
     */
    createAgentSuppressionRequest: CreateAgentSuppressionRequest
}

export interface AgentsApiDeleteAgentRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApideleteAgent
     */
    email: string
    /**
     * Must be the literal DELETE. The default action moves the agent to trash; permanent&#x3D;true is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof AgentsApideleteAgent
     */
    confirm: 'DELETE'
    /**
     * Delete irreversibly right away instead of moving to the trash. Accepts live and trashed agents.
     * Defaults to: undefined
     * @type boolean
     * @memberof AgentsApideleteAgent
     */
    permanent?: boolean
}

export interface AgentsApiDeleteAgentSuppressionRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApideleteAgentSuppression
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApideleteAgentSuppression
     */
    address: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof AgentsApideleteAgentSuppression
     */
    confirm: 'DELETE'
}

export interface AgentsApiGetAgentRequest {
    /**
     * The agent\&#39;s full email address, e.g. support@acme.com.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApigetAgent
     */
    email: string
}

export interface AgentsApiGetAgentProtectionRequest {
    /**
     * The agent\&#39;s full email address.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApigetAgentProtection
     */
    email: string
}

export interface AgentsApiListAgentSuppressionsRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApilistAgentSuppressions
     */
    email: string
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApilistAgentSuppressions
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof AgentsApilistAgentSuppressions
     */
    limit?: number
}

export interface AgentsApiListAgentsRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApilistAgents
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof AgentsApilistAgents
     */
    limit?: number
    /**
     * List the trash instead: agents that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live agents only).
     * Defaults to: undefined
     * @type boolean
     * @memberof AgentsApilistAgents
     */
    deleted?: boolean
}

export interface AgentsApiPutAgentProtectionRequest {
    /**
     * The agent\&#39;s full email address.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApiputAgentProtection
     */
    email: string
    /**
     *
     * @type ProtectionConfigRequest
     * @memberof AgentsApiputAgentProtection
     */
    protectionConfigRequest: ProtectionConfigRequest
}

export interface AgentsApiRestoreAgentRequest {
    /**
     * The agent\&#39;s full email address, e.g. support@acme.com.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApirestoreAgent
     */
    email: string
}

export interface AgentsApiTestAgentRequest {
    /**
     * The agent\&#39;s full email address, e.g. support@acme.com.
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApitestAgent
     */
    email: string
}

export interface AgentsApiUpdateAgentRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof AgentsApiupdateAgent
     */
    email: string
    /**
     *
     * @type UpdateAgentRequest
     * @memberof AgentsApiupdateAgent
     */
    updateAgentRequest: UpdateAgentRequest
}

export class ObjectAgentsApi {
    private api: ObservableAgentsApi

    public constructor(configuration: Configuration, requestFactory?: AgentsApiRequestFactory, responseProcessor?: AgentsApiResponseProcessor) {
        this.api = new ObservableAgentsApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Register an agent by full email. A custom-domain agent\'s domain must be a verified domain the caller owns; an email on the deployment\'s shared domain (e.g. xyz@agents.e2a.dev) is registered as a shared-domain agent. Returns the full agent.
     * Create an agent
     * @param param the request object
     */
    public createAgentWithHttpInfo(param: AgentsApiCreateAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<AgentView>> {
        return this.api.createAgentWithHttpInfo(param.createAgentRequest,  options).toPromise();
    }

    /**
     * Register an agent by full email. A custom-domain agent\'s domain must be a verified domain the caller owns; an email on the deployment\'s shared domain (e.g. xyz@agents.e2a.dev) is registered as a shared-domain agent. Returns the full agent.
     * Create an agent
     * @param param the request object
     */
    public createAgent(param: AgentsApiCreateAgentRequest, options?: ConfigurationOptions): Promise<AgentView> {
        return this.api.createAgent(param.createAgentRequest,  options).toPromise();
    }

    /**
     * Idempotently creates a manual recipient block for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Suppress a recipient for an agent (beta)
     * @param param the request object
     */
    public createAgentSuppressionWithHttpInfo(param: AgentsApiCreateAgentSuppressionRequest, options?: ConfigurationOptions): Promise<HttpInfo<AgentSuppressionView>> {
        return this.api.createAgentSuppressionWithHttpInfo(param.email, param.createAgentSuppressionRequest,  options).toPromise();
    }

    /**
     * Idempotently creates a manual recipient block for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Suppress a recipient for an agent (beta)
     * @param param the request object
     */
    public createAgentSuppression(param: AgentsApiCreateAgentSuppressionRequest, options?: ConfigurationOptions): Promise<AgentSuppressionView> {
        return this.api.createAgentSuppression(param.email, param.createAgentSuppressionRequest,  options).toPromise();
    }

    /**
     * Move an agent the caller owns to the trash. Requires ?confirm=DELETE. A trashed agent stops receiving mail, disappears from lists, and its held messages leave the review queue; restore it via POST /v1/agents/{email}/restore within the trash retention window — 30 days by default (deployment-configurable) — after which it is purged permanently (messages included). Live message data is otherwise retained indefinitely. Pass permanent=true to skip the trash and delete irreversibly right away (accepts live and trashed agents). Returns 200 with a deletion receipt; messages_deleted is zero when the agent is moved to trash.
     * Delete an agent
     * @param param the request object
     */
    public deleteAgentWithHttpInfo(param: AgentsApiDeleteAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteAgentResult>> {
        return this.api.deleteAgentWithHttpInfo(param.email, param.confirm, param.permanent,  options).toPromise();
    }

    /**
     * Move an agent the caller owns to the trash. Requires ?confirm=DELETE. A trashed agent stops receiving mail, disappears from lists, and its held messages leave the review queue; restore it via POST /v1/agents/{email}/restore within the trash retention window — 30 days by default (deployment-configurable) — after which it is purged permanently (messages included). Live message data is otherwise retained indefinitely. Pass permanent=true to skip the trash and delete irreversibly right away (accepts live and trashed agents). Returns 200 with a deletion receipt; messages_deleted is zero when the agent is moved to trash.
     * Delete an agent
     * @param param the request object
     */
    public deleteAgent(param: AgentsApiDeleteAgentRequest, options?: ConfigurationOptions): Promise<DeleteAgentResult> {
        return this.api.deleteAgent(param.email, param.confirm, param.permanent,  options).toPromise();
    }

    /**
     * Removes only the exact agent-scoped block. Requires ?confirm=DELETE. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Remove an agent recipient suppression (beta)
     * @param param the request object
     */
    public deleteAgentSuppressionWithHttpInfo(param: AgentsApiDeleteAgentSuppressionRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteSuppressionResult>> {
        return this.api.deleteAgentSuppressionWithHttpInfo(param.email, param.address, param.confirm,  options).toPromise();
    }

    /**
     * Removes only the exact agent-scoped block. Requires ?confirm=DELETE. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * Remove an agent recipient suppression (beta)
     * @param param the request object
     */
    public deleteAgentSuppression(param: AgentsApiDeleteAgentSuppressionRequest, options?: ConfigurationOptions): Promise<DeleteSuppressionResult> {
        return this.api.deleteAgentSuppression(param.email, param.address, param.confirm,  options).toPromise();
    }

    /**
     * Fetch a single agent the authenticated account owns, by full email address.
     * Get an agent
     * @param param the request object
     */
    public getAgentWithHttpInfo(param: AgentsApiGetAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<AgentView>> {
        return this.api.getAgentWithHttpInfo(param.email,  options).toPromise();
    }

    /**
     * Fetch a single agent the authenticated account owns, by full email address.
     * Get an agent
     * @param param the request object
     */
    public getAgent(param: AgentsApiGetAgentRequest, options?: ConfigurationOptions): Promise<AgentView> {
        return this.api.getAgent(param.email,  options).toPromise();
    }

    /**
     * Read the agent\'s protection posture — inbound/outbound trust gate, content-scan sensitivity, and hold-queue mechanism. Account scope only: an agent-scoped credential cannot read its own protection config. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Get an agent\'s protection config (beta)
     * @param param the request object
     */
    public getAgentProtectionWithHttpInfo(param: AgentsApiGetAgentProtectionRequest, options?: ConfigurationOptions): Promise<HttpInfo<ProtectionConfigView>> {
        return this.api.getAgentProtectionWithHttpInfo(param.email,  options).toPromise();
    }

    /**
     * Read the agent\'s protection posture — inbound/outbound trust gate, content-scan sensitivity, and hold-queue mechanism. Account scope only: an agent-scoped credential cannot read its own protection config. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Get an agent\'s protection config (beta)
     * @param param the request object
     */
    public getAgentProtection(param: AgentsApiGetAgentProtectionRequest, options?: ConfigurationOptions): Promise<ProtectionConfigView> {
        return this.api.getAgentProtection(param.email,  options).toPromise();
    }

    /**
     * Lists recipient addresses blocked only for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * List an agent\'s suppressed recipients (beta)
     * @param param the request object
     */
    public listAgentSuppressionsWithHttpInfo(param: AgentsApiListAgentSuppressionsRequest, options?: ConfigurationOptions): Promise<HttpInfo<PageAgentSuppressionView>> {
        return this.api.listAgentSuppressionsWithHttpInfo(param.email, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Lists recipient addresses blocked only for this exact sending agent. Account-scoped credentials only. Beta: agent-scoped suppression management may change before it is declared stable.
     * List an agent\'s suppressed recipients (beta)
     * @param param the request object
     */
    public listAgentSuppressions(param: AgentsApiListAgentSuppressionsRequest, options?: ConfigurationOptions): Promise<PageAgentSuppressionView> {
        return this.api.listAgentSuppressions(param.email, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the agents owned by the authenticated account, newest first, with cursor pagination. Pass deleted=true for the trash (soft-deleted agents, restorable until purged).
     * List agents
     * @param param the request object
     */
    public listAgentsWithHttpInfo(param: AgentsApiListAgentsRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageAgentView>> {
        return this.api.listAgentsWithHttpInfo(param.cursor, param.limit, param.deleted,  options).toPromise();
    }

    /**
     * List the agents owned by the authenticated account, newest first, with cursor pagination. Pass deleted=true for the trash (soft-deleted agents, restorable until purged).
     * List agents
     * @param param the request object
     */
    public listAgents(param: AgentsApiListAgentsRequest = {}, options?: ConfigurationOptions): Promise<PageAgentView> {
        return this.api.listAgents(param.cursor, param.limit, param.deleted,  options).toPromise();
    }

    /**
     * Replace the agent\'s protection posture wholesale. The three top-level keys (inbound, outbound, holds) are required; leaves default. Account scope only. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Replace an agent\'s protection config (beta)
     * @param param the request object
     */
    public putAgentProtectionWithHttpInfo(param: AgentsApiPutAgentProtectionRequest, options?: ConfigurationOptions): Promise<HttpInfo<ProtectionConfigView>> {
        return this.api.putAgentProtectionWithHttpInfo(param.email, param.protectionConfigRequest,  options).toPromise();
    }

    /**
     * Replace the agent\'s protection posture wholesale. The three top-level keys (inbound, outbound, holds) are required; leaves default. Account scope only. Beta: the agent protection config is unstable — its shape may change before it is declared stable.
     * Replace an agent\'s protection config (beta)
     * @param param the request object
     */
    public putAgentProtection(param: AgentsApiPutAgentProtectionRequest, options?: ConfigurationOptions): Promise<ProtectionConfigView> {
        return this.api.putAgentProtection(param.email, param.protectionConfigRequest,  options).toPromise();
    }

    /**
     * Bring a trashed (soft-deleted) agent back into service, messages and configuration intact. Live message retention is indefinite. For drafts still held for review, approval_expires_at is shifted forward by the time the agent spent in trash so a review hold cannot lapse while the inbox is unavailable. Returns the restored agent. 409 not_in_trash when the agent is not in the trash.
     * Restore an agent from the trash
     * @param param the request object
     */
    public restoreAgentWithHttpInfo(param: AgentsApiRestoreAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<AgentView>> {
        return this.api.restoreAgentWithHttpInfo(param.email,  options).toPromise();
    }

    /**
     * Bring a trashed (soft-deleted) agent back into service, messages and configuration intact. Live message retention is indefinite. For drafts still held for review, approval_expires_at is shifted forward by the time the agent spent in trash so a review hold cannot lapse while the inbox is unavailable. Returns the restored agent. 409 not_in_trash when the agent is not in the trash.
     * Restore an agent from the trash
     * @param param the request object
     */
    public restoreAgent(param: AgentsApiRestoreAgentRequest, options?: ConfigurationOptions): Promise<AgentView> {
        return this.api.restoreAgent(param.email,  options).toPromise();
    }

    /**
     * Send a platform-originated test email (From: the platform noreply identity) to the agent\'s own address over the real external SMTP route, to confirm inbound delivery end to end. Returns 202: status=accepted (the message is durably persisted and queued; message_id is the GET-able e2a message id, and the terminal outcome arrives via GET /v1/messages/{id} or the email.sent / email.failed webhook events — provider_message_id appears only after provider submission) or status=pending_review when held for review. Always branch on body.status.
     * Send a test email to the agent\'s own address
     * @param param the request object
     */
    public testAgentWithHttpInfo(param: AgentsApiTestAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<SendResultView>> {
        return this.api.testAgentWithHttpInfo(param.email,  options).toPromise();
    }

    /**
     * Send a platform-originated test email (From: the platform noreply identity) to the agent\'s own address over the real external SMTP route, to confirm inbound delivery end to end. Returns 202: status=accepted (the message is durably persisted and queued; message_id is the GET-able e2a message id, and the terminal outcome arrives via GET /v1/messages/{id} or the email.sent / email.failed webhook events — provider_message_id appears only after provider submission) or status=pending_review when held for review. Always branch on body.status.
     * Send a test email to the agent\'s own address
     * @param param the request object
     */
    public testAgent(param: AgentsApiTestAgentRequest, options?: ConfigurationOptions): Promise<SendResultView> {
        return this.api.testAgent(param.email,  options).toPromise();
    }

    /**
     * Update an agent\'s display name. The screening/protection config lives on the /v1/agents/{email}/protection sub-resource. Returns the post-update agent.
     * Update an agent
     * @param param the request object
     */
    public updateAgentWithHttpInfo(param: AgentsApiUpdateAgentRequest, options?: ConfigurationOptions): Promise<HttpInfo<AgentView>> {
        return this.api.updateAgentWithHttpInfo(param.email, param.updateAgentRequest,  options).toPromise();
    }

    /**
     * Update an agent\'s display name. The screening/protection config lives on the /v1/agents/{email}/protection sub-resource. Returns the post-update agent.
     * Update an agent
     * @param param the request object
     */
    public updateAgent(param: AgentsApiUpdateAgentRequest, options?: ConfigurationOptions): Promise<AgentView> {
        return this.api.updateAgent(param.email, param.updateAgentRequest,  options).toPromise();
    }

}

import { ObservableConversationsApi } from "./ObservableAPI.js";
import { ConversationsApiRequestFactory, ConversationsApiResponseProcessor} from "../apis/ConversationsApi.js";

export interface ConversationsApiGetConversationRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApigetConversation
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApigetConversation
     */
    id: string
}

export interface ConversationsApiListConversationsRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApilistConversations
     */
    email: string
    /**
     * RFC3339.
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApilistConversations
     */
    since?: string
    /**
     * RFC3339.
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApilistConversations
     */
    until?: string
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change since/until.
     * Defaults to: undefined
     * @type string
     * @memberof ConversationsApilistConversations
     */
    cursor?: string
    /**
     *
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof ConversationsApilistConversations
     */
    limit?: number
}

export class ObjectConversationsApi {
    private api: ObservableConversationsApi

    public constructor(configuration: Configuration, requestFactory?: ConversationsApiRequestFactory, responseProcessor?: ConversationsApiResponseProcessor) {
        this.api = new ObservableConversationsApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Fetch a single conversation thread with its participants, labels, and member messages.
     * Get a conversation
     * @param param the request object
     */
    public getConversationWithHttpInfo(param: ConversationsApiGetConversationRequest, options?: ConfigurationOptions): Promise<HttpInfo<ConversationDetailView>> {
        return this.api.getConversationWithHttpInfo(param.email, param.id,  options).toPromise();
    }

    /**
     * Fetch a single conversation thread with its participants, labels, and member messages.
     * Get a conversation
     * @param param the request object
     */
    public getConversation(param: ConversationsApiGetConversationRequest, options?: ConfigurationOptions): Promise<ConversationDetailView> {
        return this.api.getConversation(param.email, param.id,  options).toPromise();
    }

    /**
     * List an agent\'s conversation threads (derived from messages.conversation_id).
     * List conversations
     * @param param the request object
     */
    public listConversationsWithHttpInfo(param: ConversationsApiListConversationsRequest, options?: ConfigurationOptions): Promise<HttpInfo<PageConversationSummaryView>> {
        return this.api.listConversationsWithHttpInfo(param.email, param.since, param.until, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List an agent\'s conversation threads (derived from messages.conversation_id).
     * List conversations
     * @param param the request object
     */
    public listConversations(param: ConversationsApiListConversationsRequest, options?: ConfigurationOptions): Promise<PageConversationSummaryView> {
        return this.api.listConversations(param.email, param.since, param.until, param.cursor, param.limit,  options).toPromise();
    }

}

import { ObservableDomainsApi } from "./ObservableAPI.js";
import { DomainsApiRequestFactory, DomainsApiResponseProcessor} from "../apis/DomainsApi.js";

export interface DomainsApiDeleteDomainRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof DomainsApideleteDomain
     */
    domain: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof DomainsApideleteDomain
     */
    confirm: 'DELETE'
}

export interface DomainsApiGetDomainRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof DomainsApigetDomain
     */
    domain: string
}

export interface DomainsApiListDomainsRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof DomainsApilistDomains
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof DomainsApilistDomains
     */
    limit?: number
}

export interface DomainsApiRegisterDomainRequest {
    /**
     *
     * @type RegisterDomainRequest
     * @memberof DomainsApiregisterDomain
     */
    registerDomainRequest: RegisterDomainRequest
}

export interface DomainsApiVerifyDomainRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof DomainsApiverifyDomain
     */
    domain: string
}

export class ObjectDomainsApi {
    private api: ObservableDomainsApi

    public constructor(configuration: Configuration, requestFactory?: DomainsApiRequestFactory, responseProcessor?: DomainsApiResponseProcessor) {
        this.api = new ObservableDomainsApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Deprovisions the domain\'s sending identity and breaks sending for every agent on it. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain}).
     * Delete a domain
     * @param param the request object
     */
    public deleteDomainWithHttpInfo(param: DomainsApiDeleteDomainRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteDomainResult>> {
        return this.api.deleteDomainWithHttpInfo(param.domain, param.confirm,  options).toPromise();
    }

    /**
     * Deprovisions the domain\'s sending identity and breaks sending for every agent on it. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain}).
     * Delete a domain
     * @param param the request object
     */
    public deleteDomain(param: DomainsApiDeleteDomainRequest, options?: ConfigurationOptions): Promise<DeleteDomainResult> {
        return this.api.deleteDomain(param.domain, param.confirm,  options).toPromise();
    }

    /**
     * Get a domain
     * @param param the request object
     */
    public getDomainWithHttpInfo(param: DomainsApiGetDomainRequest, options?: ConfigurationOptions): Promise<HttpInfo<DomainView>> {
        return this.api.getDomainWithHttpInfo(param.domain,  options).toPromise();
    }

    /**
     * Get a domain
     * @param param the request object
     */
    public getDomain(param: DomainsApiGetDomainRequest, options?: ConfigurationOptions): Promise<DomainView> {
        return this.api.getDomain(param.domain,  options).toPromise();
    }

    /**
     * List the domains owned by the authenticated account, newest first, with cursor pagination.
     * List domains
     * @param param the request object
     */
    public listDomainsWithHttpInfo(param: DomainsApiListDomainsRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageDomainView>> {
        return this.api.listDomainsWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the domains owned by the authenticated account, newest first, with cursor pagination.
     * List domains
     * @param param the request object
     */
    public listDomains(param: DomainsApiListDomainsRequest = {}, options?: ConfigurationOptions): Promise<PageDomainView> {
        return this.api.listDomains(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Register a domain
     * @param param the request object
     */
    public registerDomainWithHttpInfo(param: DomainsApiRegisterDomainRequest, options?: ConfigurationOptions): Promise<HttpInfo<DomainView>> {
        return this.api.registerDomainWithHttpInfo(param.registerDomainRequest,  options).toPromise();
    }

    /**
     * Register a domain
     * @param param the request object
     */
    public registerDomain(param: DomainsApiRegisterDomainRequest, options?: ConfigurationOptions): Promise<DomainView> {
        return this.api.registerDomain(param.registerDomainRequest,  options).toPromise();
    }

    /**
     * Probe the domain\'s published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.
     * Verify a domain
     * @param param the request object
     */
    public verifyDomainWithHttpInfo(param: DomainsApiVerifyDomainRequest, options?: ConfigurationOptions): Promise<HttpInfo<VerifyDomainView>> {
        return this.api.verifyDomainWithHttpInfo(param.domain,  options).toPromise();
    }

    /**
     * Probe the domain\'s published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.
     * Verify a domain
     * @param param the request object
     */
    public verifyDomain(param: DomainsApiVerifyDomainRequest, options?: ConfigurationOptions): Promise<VerifyDomainView> {
        return this.api.verifyDomain(param.domain,  options).toPromise();
    }

}

import { ObservableEventsApi } from "./ObservableAPI.js";
import { EventsApiRequestFactory, EventsApiResponseProcessor} from "../apis/EventsApi.js";

export interface EventsApiGetEventRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApigetEvent
     */
    id: string
}

export interface EventsApiListEventsRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    type?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    agentEmail?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    conversationId?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    messageId?: string
    /**
     * RFC3339.
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    since?: string
    /**
     * RFC3339.
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    until?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApilistEvents
     */
    cursor?: string
    /**
     *
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof EventsApilistEvents
     */
    limit?: number
}

export interface EventsApiRedeliverEventRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof EventsApiredeliverEvent
     */
    id: string
    /**
     *
     * @type RedeliverEventRequest
     * @memberof EventsApiredeliverEvent
     */
    redeliverEventRequest: RedeliverEventRequest
}

export class ObjectEventsApi {
    private api: ObservableEventsApi

    public constructor(configuration: Configuration, requestFactory?: EventsApiRequestFactory, responseProcessor?: EventsApiResponseProcessor) {
        this.api = new ObservableEventsApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Get an event
     * @param param the request object
     */
    public getEventWithHttpInfo(param: EventsApiGetEventRequest, options?: ConfigurationOptions): Promise<HttpInfo<EventView>> {
        return this.api.getEventWithHttpInfo(param.id,  options).toPromise();
    }

    /**
     * Get an event
     * @param param the request object
     */
    public getEvent(param: EventsApiGetEventRequest, options?: ConfigurationOptions): Promise<EventView> {
        return this.api.getEvent(param.id,  options).toPromise();
    }

    /**
     * The webhook-event delivery log, filterable by type/agent/conversation/message and time range, with cursor pagination.
     * List events
     * @param param the request object
     */
    public listEventsWithHttpInfo(param: EventsApiListEventsRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageEventView>> {
        return this.api.listEventsWithHttpInfo(param.type, param.agentEmail, param.conversationId, param.messageId, param.since, param.until, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * The webhook-event delivery log, filterable by type/agent/conversation/message and time range, with cursor pagination.
     * List events
     * @param param the request object
     */
    public listEvents(param: EventsApiListEventsRequest = {}, options?: ConfigurationOptions): Promise<PageEventView> {
        return this.api.listEvents(param.type, param.agentEmail, param.conversationId, param.messageId, param.since, param.until, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Re-enqueue webhook delivery for an event. With a webhook_id, replays to that subscriber; without, fans out to every originally-matched subscriber. Auto-deduplicated within a short window — receivers must dedup on event id. Returns 202 Accepted: the redelivery is durably enqueued for async submission, not delivered synchronously — the per-subscriber outcome surfaces via the delivery log, and each delivery\'s status is \'pending\' (or \'scheduled\' for the fan-out).
     * Redeliver an event
     * @param param the request object
     */
    public redeliverEventWithHttpInfo(param: EventsApiRedeliverEventRequest, options?: ConfigurationOptions): Promise<HttpInfo<RedeliverView>> {
        return this.api.redeliverEventWithHttpInfo(param.id, param.redeliverEventRequest,  options).toPromise();
    }

    /**
     * Re-enqueue webhook delivery for an event. With a webhook_id, replays to that subscriber; without, fans out to every originally-matched subscriber. Auto-deduplicated within a short window — receivers must dedup on event id. Returns 202 Accepted: the redelivery is durably enqueued for async submission, not delivered synchronously — the per-subscriber outcome surfaces via the delivery log, and each delivery\'s status is \'pending\' (or \'scheduled\' for the fan-out).
     * Redeliver an event
     * @param param the request object
     */
    public redeliverEvent(param: EventsApiRedeliverEventRequest, options?: ConfigurationOptions): Promise<RedeliverView> {
        return this.api.redeliverEvent(param.id, param.redeliverEventRequest,  options).toPromise();
    }

}

import { ObservableMessagesApi } from "./ObservableAPI.js";
import { MessagesApiRequestFactory, MessagesApiResponseProcessor} from "../apis/MessagesApi.js";

export interface MessagesApiDeleteMessageRequest {
    /**
     * The agent\&#39;s full email address.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApideleteMessage
     */
    email: string
    /**
     * The message id, e.g. msg_abc123.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApideleteMessage
     */
    id: string
    /**
     * Permanently delete a message that is already in the trash (irreversible). Requires confirm&#x3D;DELETE and an account-scoped credential.
     * Defaults to: undefined
     * @type boolean
     * @memberof MessagesApideleteMessage
     */
    permanent?: boolean
    /**
     * Must be the literal string DELETE when permanent&#x3D;true; ignored otherwise.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApideleteMessage
     */
    confirm?: string
}

export interface MessagesApiForwardMessageRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiforwardMessage
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiforwardMessage
     */
    id: string
    /**
     *
     * @type ForwardRequest
     * @memberof MessagesApiforwardMessage
     */
    forwardRequest: ForwardRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiforwardMessage
     */
    idempotencyKey?: string
    /**
     * Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiforwardMessage
     */
    wait?: string
}

export interface MessagesApiGetAttachmentRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetAttachment
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetAttachment
     */
    id: string
    /**
     *
     * Minimum: 0
     * Defaults to: undefined
     * @type number
     * @memberof MessagesApigetAttachment
     */
    index: number
    /**
     * When true, also include the bytes as base64 in \&#39;data\&#39; — ONLY for attachments &lt;&#x3D; 256 KB; larger inline requests are rejected (413). Default false (use download_url).
     * Defaults to: undefined
     * @type boolean
     * @memberof MessagesApigetAttachment
     */
    inline?: boolean
}

export interface MessagesApiGetMessageRequest {
    /**
     * The agent\&#39;s full email address.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetMessage
     */
    email: string
    /**
     * The message id, e.g. msg_abc123.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetMessage
     */
    id: string
}

export interface MessagesApiGetMessageLifecycleRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetMessageLifecycle
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetMessageLifecycle
     */
    id: string
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApigetMessageLifecycle
     */
    cursor?: string
    /**
     * Maximum number of lifecycle transitions to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 50
     * @type number
     * @memberof MessagesApigetMessageLifecycle
     */
    limit?: number
}

export interface MessagesApiListMessagesRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    email: string
    /**
     * Defaults to inbound.
     * Defaults to: undefined
     * @type &#39;inbound&#39; | &#39;outbound&#39; | &#39;all&#39;
     * @memberof MessagesApilistMessages
     */
    direction?: 'inbound' | 'outbound' | 'all'
    /**
     * Inbound only. Filters by inbox read-state (MSG-1). Defaults to unread for inbound, all otherwise.
     * Defaults to: undefined
     * @type &#39;unread&#39; | &#39;read&#39; | &#39;all&#39;
     * @memberof MessagesApilistMessages
     */
    readStatus?: 'unread' | 'read' | 'all'
    /**
     * Defaults to desc (newest first).
     * Defaults to: undefined
     * @type &#39;asc&#39; | &#39;desc&#39;
     * @memberof MessagesApilistMessages
     */
    sort?: 'asc' | 'desc'
    /**
     * Case-insensitive substring match on sender.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    from_?: string
    /**
     * Case-insensitive substring match on subject.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    subjectContains?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    conversationId?: string
    /**
     * Comma-separated list (e.g. labels&#x3D;urgent,follow-up); AND-matched — a message must carry every given label.
     * Defaults to: undefined
     * @type Array&lt;string&gt;
     * @memberof MessagesApilistMessages
     */
    labels?: Array<string>
    /**
     * RFC3339; created_at &gt;&#x3D; since.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    since?: string
    /**
     * RFC3339; created_at &lt; until.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    until?: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApilistMessages
     */
    cursor?: string
    /**
     *
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof MessagesApilistMessages
     */
    limit?: number
    /**
     * List the trash instead: messages that were soft-deleted and are restorable until purged (30 days after deletion by default, deployment-configurable). Defaults to false (live messages only).
     * Defaults to: undefined
     * @type boolean
     * @memberof MessagesApilistMessages
     */
    deleted?: boolean
}

export interface MessagesApiReplyToMessageRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApireplyToMessage
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApireplyToMessage
     */
    id: string
    /**
     *
     * @type ReplyRequest
     * @memberof MessagesApireplyToMessage
     */
    replyRequest: ReplyRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApireplyToMessage
     */
    idempotencyKey?: string
    /**
     * Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApireplyToMessage
     */
    wait?: string
}

export interface MessagesApiRestoreMessageRequest {
    /**
     * The agent\&#39;s full email address.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApirestoreMessage
     */
    email: string
    /**
     * The message id, e.g. msg_abc123.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApirestoreMessage
     */
    id: string
}

export interface MessagesApiSendMessageRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApisendMessage
     */
    email: string
    /**
     *
     * @type SendEmailRequest
     * @memberof MessagesApisendMessage
     */
    sendEmailRequest: SendEmailRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. If the response is lost after durable acceptance, retry with the same key and byte-identical body to recover the original 202 and message ID; retrying without a key can enqueue a duplicate. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort under idempotency-store degradation before atomic acceptance; accepted keyed sends commit their message, River job, and replay response together.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApisendMessage
     */
    idempotencyKey?: string
    /**
     * Optional bounded wait. wait&#x3D;sent holds the request until the asynchronously delivered message reaches a terminal-or-held state or at most 20 seconds elapse (currently ~15s), then returns the observed state; on timeout returns status&#x3D;accepted. Default: no wait. Always branch on body.status, not the HTTP code.
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApisendMessage
     */
    wait?: string
}

export interface MessagesApiUpdateMessageRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiupdateMessage
     */
    email: string
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof MessagesApiupdateMessage
     */
    id: string
    /**
     *
     * @type UpdateMessageRequest
     * @memberof MessagesApiupdateMessage
     */
    updateMessageRequest: UpdateMessageRequest
}

export class ObjectMessagesApi {
    private api: ObservableMessagesApi

    public constructor(configuration: Configuration, requestFactory?: MessagesApiRequestFactory, responseProcessor?: MessagesApiResponseProcessor) {
        this.api = new ObservableMessagesApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Move a message to the trash. Trashed messages disappear from lists, threads, and reply targets, but can be restored via POST …/messages/{id}/restore until they are purged — 30 days after deletion by default (the trash retention window is deployment-configurable). Live message data is otherwise retained indefinitely. No confirmation is required because the default delete is reversible. Pass permanent=true with confirm=DELETE to permanently delete a message that is ALREADY in the trash (\"delete forever\"). A message held for review (review_status=pending_review) cannot be deleted — resolve it in the review queue first (409 message_held).
     * Delete a message (move to trash)
     * @param param the request object
     */
    public deleteMessageWithHttpInfo(param: MessagesApiDeleteMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteMessageResult>> {
        return this.api.deleteMessageWithHttpInfo(param.email, param.id, param.permanent, param.confirm,  options).toPromise();
    }

    /**
     * Move a message to the trash. Trashed messages disappear from lists, threads, and reply targets, but can be restored via POST …/messages/{id}/restore until they are purged — 30 days after deletion by default (the trash retention window is deployment-configurable). Live message data is otherwise retained indefinitely. No confirmation is required because the default delete is reversible. Pass permanent=true with confirm=DELETE to permanently delete a message that is ALREADY in the trash (\"delete forever\"). A message held for review (review_status=pending_review) cannot be deleted — resolve it in the review queue first (409 message_held).
     * Delete a message (move to trash)
     * @param param the request object
     */
    public deleteMessage(param: MessagesApiDeleteMessageRequest, options?: ConfigurationOptions): Promise<DeleteMessageResult> {
        return this.api.deleteMessage(param.email, param.id, param.permanent, param.confirm,  options).toPromise();
    }

    /**
     * Forward a message (inbound or outbound) to new recipients; the original is quoted and its attachments are carried over by default. Any attachments[] you supply are added on top of the originals. 202 when held for HITL. Forwarding a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — a forward requires the source message to have actually been sent; retry once it is sent, or use wait=sent on the original send. Attachment limits apply to the combined set (carried-over originals + supplied): at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Forward a message
     * @param param the request object
     */
    public forwardMessageWithHttpInfo(param: MessagesApiForwardMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<SendResultView>> {
        return this.api.forwardMessageWithHttpInfo(param.email, param.id, param.forwardRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Forward a message (inbound or outbound) to new recipients; the original is quoted and its attachments are carried over by default. Any attachments[] you supply are added on top of the originals. 202 when held for HITL. Forwarding a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — a forward requires the source message to have actually been sent; retry once it is sent, or use wait=sent on the original send. Attachment limits apply to the combined set (carried-over originals + supplied): at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Forward a message
     * @param param the request object
     */
    public forwardMessage(param: MessagesApiForwardMessageRequest, options?: ConfigurationOptions): Promise<SendResultView> {
        return this.api.forwardMessage(param.email, param.id, param.forwardRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Returns one attachment\'s metadata plus a short-lived `download_url` (+ `expires_at`) to fetch the bytes out of band — so binary content never streams through an agent\'s context. Pass `?inline=true` to also receive base64 `data` for small attachments (<= 256 KB); larger inline requests are rejected with 413 attachment_too_large. `index` is the 0-based attachment index from the message\'s `attachments[]`.
     * Get an attachment (metadata + short-lived download URL)
     * @param param the request object
     */
    public getAttachmentWithHttpInfo(param: MessagesApiGetAttachmentRequest, options?: ConfigurationOptions): Promise<HttpInfo<AttachmentView>> {
        return this.api.getAttachmentWithHttpInfo(param.email, param.id, param.index, param.inline,  options).toPromise();
    }

    /**
     * Returns one attachment\'s metadata plus a short-lived `download_url` (+ `expires_at`) to fetch the bytes out of band — so binary content never streams through an agent\'s context. Pass `?inline=true` to also receive base64 `data` for small attachments (<= 256 KB); larger inline requests are rejected with 413 attachment_too_large. `index` is the 0-based attachment index from the message\'s `attachments[]`.
     * Get an attachment (metadata + short-lived download URL)
     * @param param the request object
     */
    public getAttachment(param: MessagesApiGetAttachmentRequest, options?: ConfigurationOptions): Promise<AttachmentView> {
        return this.api.getAttachment(param.email, param.id, param.index, param.inline,  options).toPromise();
    }

    /**
     * Fetch a single message (inbound or outbound) by id, scoped to an agent the caller owns. A trashed message remains readable by this direct GET and includes deleted_at until it is permanently purged (30 days after deletion by default, deployment-configurable); ordinary lists, conversations, reply targets, and forward targets exclude it. Includes the raw message and canonical inbound authentication evidence. Fetching an unread inbound message marks it read as a side effect.
     * Get a message
     * @param param the request object
     */
    public getMessageWithHttpInfo(param: MessagesApiGetMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<MessageView>> {
        return this.api.getMessageWithHttpInfo(param.email, param.id,  options).toPromise();
    }

    /**
     * Fetch a single message (inbound or outbound) by id, scoped to an agent the caller owns. A trashed message remains readable by this direct GET and includes deleted_at until it is permanently purged (30 days after deletion by default, deployment-configurable); ordinary lists, conversations, reply targets, and forward targets exclude it. Includes the raw message and canonical inbound authentication evidence. Fetching an unread inbound message marks it read as a side effect.
     * Get a message
     * @param param the request object
     */
    public getMessage(param: MessagesApiGetMessageRequest, options?: ConfigurationOptions): Promise<MessageView> {
        return this.api.getMessage(param.email, param.id,  options).toPromise();
    }

    /**
     * Returns the observations e2a recorded for one inbound or outbound message in deterministic ascending (occurred_at, id) order. Delivery means recipient-server acceptance and does not claim inbox placement. Beta: message lifecycle may change before it is declared stable.
     * Get a message\'s lifecycle (beta)
     * @param param the request object
     */
    public getMessageLifecycleWithHttpInfo(param: MessagesApiGetMessageLifecycleRequest, options?: ConfigurationOptions): Promise<HttpInfo<PageMessageLifecycleTransition>> {
        return this.api.getMessageLifecycleWithHttpInfo(param.email, param.id, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Returns the observations e2a recorded for one inbound or outbound message in deterministic ascending (occurred_at, id) order. Delivery means recipient-server acceptance and does not claim inbox placement. Beta: message lifecycle may change before it is declared stable.
     * Get a message\'s lifecycle (beta)
     * @param param the request object
     */
    public getMessageLifecycle(param: MessagesApiGetMessageLifecycleRequest, options?: ConfigurationOptions): Promise<PageMessageLifecycleTransition> {
        return this.api.getMessageLifecycle(param.email, param.id, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List an agent\'s messages (inbound + outbound) with filters and cursor pagination. Held outbound drafts appear as status=pending_review. Pass deleted=true for the trash (soft-deleted messages, restorable until purged — 30 days after deletion by default, deployment-configurable); the trash view defaults to direction=all and read_status=all.
     * List messages
     * @param param the request object
     */
    public listMessagesWithHttpInfo(param: MessagesApiListMessagesRequest, options?: ConfigurationOptions): Promise<HttpInfo<PageMessageSummaryView>> {
        return this.api.listMessagesWithHttpInfo(param.email, param.direction, param.readStatus, param.sort, param.from_, param.subjectContains, param.conversationId, param.labels, param.since, param.until, param.cursor, param.limit, param.deleted,  options).toPromise();
    }

    /**
     * List an agent\'s messages (inbound + outbound) with filters and cursor pagination. Held outbound drafts appear as status=pending_review. Pass deleted=true for the trash (soft-deleted messages, restorable until purged — 30 days after deletion by default, deployment-configurable); the trash view defaults to direction=all and read_status=all.
     * List messages
     * @param param the request object
     */
    public listMessages(param: MessagesApiListMessagesRequest, options?: ConfigurationOptions): Promise<PageMessageSummaryView> {
        return this.api.listMessages(param.email, param.direction, param.readStatus, param.sort, param.from_, param.subjectContains, param.conversationId, param.labels, param.since, param.until, param.cursor, param.limit, param.deleted,  options).toPromise();
    }

    /**
     * Reply to a message (inbound or outbound); recipients and threading are derived from the original. Replying to a message the agent received targets its sender; replying to a message the agent sent continues the thread to its original recipients (`reply_all` also re-includes the original Cc). 202 when held for HITL. Replying to a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — it has no Message-ID to thread onto; retry once it is sent, or use wait=sent on the original send. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Reply to a message
     * @param param the request object
     */
    public replyToMessageWithHttpInfo(param: MessagesApiReplyToMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<SendResultView>> {
        return this.api.replyToMessageWithHttpInfo(param.email, param.id, param.replyRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Reply to a message (inbound or outbound); recipients and threading are derived from the original. Replying to a message the agent received targets its sender; replying to a message the agent sent continues the thread to its original recipients (`reply_all` also re-includes the original Cc). 202 when held for HITL. Replying to a message this agent sent that has not been submitted to the provider yet returns 409 message_not_yet_delivered — it has no Message-ID to thread onto; retry once it is sent, or use wait=sent on the original send. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large.
     * Reply to a message
     * @param param the request object
     */
    public replyToMessage(param: MessagesApiReplyToMessageRequest, options?: ConfigurationOptions): Promise<SendResultView> {
        return this.api.replyToMessage(param.email, param.id, param.replyRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Bring a trashed (soft-deleted) message back to the inbox. Restored message data is retained indefinitely unless it is deleted again. Returns the restored message. 409 not_in_trash when the message is not in the trash.
     * Restore a message from the trash
     * @param param the request object
     */
    public restoreMessageWithHttpInfo(param: MessagesApiRestoreMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<MessageView>> {
        return this.api.restoreMessageWithHttpInfo(param.email, param.id,  options).toPromise();
    }

    /**
     * Bring a trashed (soft-deleted) message back to the inbox. Restored message data is retained indefinitely unless it is deleted again. Returns the restored message. 409 not_in_trash when the message is not in the trash.
     * Restore a message from the trash
     * @param param the request object
     */
    public restoreMessage(param: MessagesApiRestoreMessageRequest, options?: ConfigurationOptions): Promise<MessageView> {
        return this.api.restoreMessage(param.email, param.id,  options).toPromise();
    }

    /**
     * Send a new email from the agent named in the path (a new thread). The sender is the path agent — `reply`/`forward` are their own sub-resources. 202 + pending_review when the agent has HITL enabled. Honors Idempotency-Key. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. Two capacity limits apply and are permanently distinct — branch on the HTTP status: 402 limit_exceeded is a QUOTA (monthly-message / storage stock-or-flow cap; a retry will not clear it — surface an upgrade path), 429 rate_limited is a throughput/request-RATE cap (transient; back off Retry-After seconds and retry).
     * Send a new email
     * @param param the request object
     */
    public sendMessageWithHttpInfo(param: MessagesApiSendMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<SendResultView>> {
        return this.api.sendMessageWithHttpInfo(param.email, param.sendEmailRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Send a new email from the agent named in the path (a new thread). The sender is the path agent — `reply`/`forward` are their own sub-resources. 202 + pending_review when the agent has HITL enabled. Honors Idempotency-Key. Attachment limits: at most 10 attachments, each ≤ 10 MiB decoded, ≤ 25 MiB decoded combined (over-count → 400 invalid_request; over-size → 413 payload_too_large). Composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. Two capacity limits apply and are permanently distinct — branch on the HTTP status: 402 limit_exceeded is a QUOTA (monthly-message / storage stock-or-flow cap; a retry will not clear it — surface an upgrade path), 429 rate_limited is a throughput/request-RATE cap (transient; back off Retry-After seconds and retry).
     * Send a new email
     * @param param the request object
     */
    public sendMessage(param: MessagesApiSendMessageRequest, options?: ConfigurationOptions): Promise<SendResultView> {
        return this.api.sendMessage(param.email, param.sendEmailRequest, param.idempotencyKey, param.wait,  options).toPromise();
    }

    /**
     * Apply a labels delta (`add_labels` / `remove_labels`) to a message the caller owns; returns the post-update label set. Each list is capped at 50 entries; labels are lowercase `[a-z0-9:_-]+` up to 64 chars; the `e2a:` prefix is reserved for system labels. A message carries at most 100 labels. An empty delta is a read of the current labels.
     * Update a message (labels)
     * @param param the request object
     */
    public updateMessageWithHttpInfo(param: MessagesApiUpdateMessageRequest, options?: ConfigurationOptions): Promise<HttpInfo<UpdateMessageResultView>> {
        return this.api.updateMessageWithHttpInfo(param.email, param.id, param.updateMessageRequest,  options).toPromise();
    }

    /**
     * Apply a labels delta (`add_labels` / `remove_labels`) to a message the caller owns; returns the post-update label set. Each list is capped at 50 entries; labels are lowercase `[a-z0-9:_-]+` up to 64 chars; the `e2a:` prefix is reserved for system labels. A message carries at most 100 labels. An empty delta is a read of the current labels.
     * Update a message (labels)
     * @param param the request object
     */
    public updateMessage(param: MessagesApiUpdateMessageRequest, options?: ConfigurationOptions): Promise<UpdateMessageResultView> {
        return this.api.updateMessage(param.email, param.id, param.updateMessageRequest,  options).toPromise();
    }

}

import { ObservableMetaApi } from "./ObservableAPI.js";
import { MetaApiRequestFactory, MetaApiResponseProcessor} from "../apis/MetaApi.js";

export interface MetaApiGetInfoRequest {
}

export class ObjectMetaApi {
    private api: ObservableMetaApi

    public constructor(configuration: Configuration, requestFactory?: MetaApiRequestFactory, responseProcessor?: MetaApiResponseProcessor) {
        this.api = new ObservableMetaApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Public deployment metadata: the shared agent domain (if slug registration is enabled) and the public base URL. Unauthenticated.
     * Deployment info
     * @param param the request object
     */
    public getInfoWithHttpInfo(param: MetaApiGetInfoRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<DeploymentInfoView>> {
        return this.api.getInfoWithHttpInfo( options).toPromise();
    }

    /**
     * Public deployment metadata: the shared agent domain (if slug registration is enabled) and the public base URL. Unauthenticated.
     * Deployment info
     * @param param the request object
     */
    public getInfo(param: MetaApiGetInfoRequest = {}, options?: ConfigurationOptions): Promise<DeploymentInfoView> {
        return this.api.getInfo( options).toPromise();
    }

}

import { ObservableReviewsApi } from "./ObservableAPI.js";
import { ReviewsApiRequestFactory, ReviewsApiResponseProcessor} from "../apis/ReviewsApi.js";

export interface ReviewsApiApproveReviewRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ReviewsApiapproveReview
     */
    id: string
    /**
     *
     * @type ApproveRequest
     * @memberof ReviewsApiapproveReview
     */
    approveRequest: ApproveRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     * Defaults to: undefined
     * @type string
     * @memberof ReviewsApiapproveReview
     */
    idempotencyKey?: string
}

export interface ReviewsApiGetReviewRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ReviewsApigetReview
     */
    id: string
}

export interface ReviewsApiListReviewsRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof ReviewsApilistReviews
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof ReviewsApilistReviews
     */
    limit?: number
}

export interface ReviewsApiRejectReviewRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof ReviewsApirejectReview
     */
    id: string
    /**
     *
     * @type RejectRequest
     * @memberof ReviewsApirejectReview
     */
    rejectRequest: RejectRequest
}

export class ObjectReviewsApi {
    private api: ObservableReviewsApi

    public constructor(configuration: Configuration, requestFactory?: ReviewsApiRequestFactory, responseProcessor?: ReviewsApiResponseProcessor) {
        this.api = new ObservableReviewsApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Approve a hold. Branches on direction: an outbound draft is durably queued for asynchronous delivery (honoring Idempotency-Key + optional reviewer overrides); an inbound hold is released to the inbox. Returns 202 with status=accepted for queued outbound delivery and 200 for an inbound release or local self-send loopback. Account-scoped only — an agent cannot approve its own hold. Approving an outbound draft applies the same per-agent send-rate limit as a direct send: 429 rate_limited when the agent is over its throughput limit (back off Retry-After seconds and retry). The merged outbound draft after applying reviewer overrides is subject to the same composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. The final merged recipient set (to, cc, and bcc, including reviewer overrides) is also re-checked against the account suppression list: any suppressed recipient returns 422 recipient_suppressed and the hold stays pending_review — remove the suppression (DELETE /v1/account/suppressions/{address}) and approve again. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Approve a held message (beta)
     * @param param the request object
     */
    public approveReviewWithHttpInfo(param: ReviewsApiApproveReviewRequest, options?: ConfigurationOptions): Promise<HttpInfo<SendResultView>> {
        return this.api.approveReviewWithHttpInfo(param.id, param.approveRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Approve a hold. Branches on direction: an outbound draft is durably queued for asynchronous delivery (honoring Idempotency-Key + optional reviewer overrides); an inbound hold is released to the inbox. Returns 202 with status=accepted for queued outbound delivery and 200 for an inbound release or local self-send loopback. Account-scoped only — an agent cannot approve its own hold. Approving an outbound draft applies the same per-agent send-rate limit as a direct send: 429 rate_limited when the agent is over its throughput limit (back off Retry-After seconds and retry). The merged outbound draft after applying reviewer overrides is subject to the same composed-message ceiling: 10 MiB (10485760 bytes), measured as subject + text + html + decoded attachment bytes; exceeding it returns 413 payload_too_large. The final merged recipient set (to, cc, and bcc, including reviewer overrides) is also re-checked against the account suppression list: any suppressed recipient returns 422 recipient_suppressed and the hold stays pending_review — remove the suppression (DELETE /v1/account/suppressions/{address}) and approve again. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Approve a held message (beta)
     * @param param the request object
     */
    public approveReview(param: ReviewsApiApproveReviewRequest, options?: ConfigurationOptions): Promise<SendResultView> {
        return this.api.approveReview(param.id, param.approveRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Full detail of one held message — body + recipients (and, for inbound, the screening/auth context) — for a reviewer to make a decision. Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Get a held message (full detail, beta)
     * @param param the request object
     */
    public getReviewWithHttpInfo(param: ReviewsApiGetReviewRequest, options?: ConfigurationOptions): Promise<HttpInfo<MessageView>> {
        return this.api.getReviewWithHttpInfo(param.id,  options).toPromise();
    }

    /**
     * Full detail of one held message — body + recipients (and, for inbound, the screening/auth context) — for a reviewer to make a decision. Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Get a held message (full detail, beta)
     * @param param the request object
     */
    public getReview(param: ReviewsApiGetReviewRequest, options?: ConfigurationOptions): Promise<MessageView> {
        return this.api.getReview(param.id,  options).toPromise();
    }

    /**
     * The review queue: every message held in pending_review across the account\'s inboxes — outbound drafts awaiting send approval AND inbound messages held by a screening gate. Account-scoped credentials only; agents cannot see (or resolve) holds. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * List messages awaiting review (beta)
     * @param param the request object
     */
    public listReviewsWithHttpInfo(param: ReviewsApiListReviewsRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageReviewView>> {
        return this.api.listReviewsWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * The review queue: every message held in pending_review across the account\'s inboxes — outbound drafts awaiting send approval AND inbound messages held by a screening gate. Account-scoped credentials only; agents cannot see (or resolve) holds. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * List messages awaiting review (beta)
     * @param param the request object
     */
    public listReviews(param: ReviewsApiListReviewsRequest = {}, options?: ConfigurationOptions): Promise<PageReviewView> {
        return this.api.listReviews(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Reject a hold. An outbound draft is discarded (never sent); an inbound hold is dropped (never reaches the agent; payload retained hidden for forensics). Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Reject a held message (beta)
     * @param param the request object
     */
    public rejectReviewWithHttpInfo(param: ReviewsApiRejectReviewRequest, options?: ConfigurationOptions): Promise<HttpInfo<RejectResultView>> {
        return this.api.rejectReviewWithHttpInfo(param.id, param.rejectRequest,  options).toPromise();
    }

    /**
     * Reject a hold. An outbound draft is discarded (never sent); an inbound hold is dropped (never reaches the agent; payload retained hidden for forensics). Account-scoped only. Beta: the unified gate/scan review resource is unstable — its shape may change before it is declared stable.
     * Reject a held message (beta)
     * @param param the request object
     */
    public rejectReview(param: ReviewsApiRejectReviewRequest, options?: ConfigurationOptions): Promise<RejectResultView> {
        return this.api.rejectReview(param.id, param.rejectRequest,  options).toPromise();
    }

}

import { ObservableTemplatesApi } from "./ObservableAPI.js";
import { TemplatesApiRequestFactory, TemplatesApiResponseProcessor} from "../apis/TemplatesApi.js";

export interface TemplatesApiCreateTemplateRequest {
    /**
     *
     * @type CreateTemplateRequest
     * @memberof TemplatesApicreateTemplate
     */
    createTemplateRequest: CreateTemplateRequest
}

export interface TemplatesApiDeleteTemplateRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApideleteTemplate
     */
    id: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof TemplatesApideleteTemplate
     */
    confirm: 'DELETE'
}

export interface TemplatesApiGetStarterTemplateRequest {
    /**
     * The starter template\&#39;s alias, e.g. welcome.
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApigetStarterTemplate
     */
    alias: string
}

export interface TemplatesApiGetTemplateRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApigetTemplate
     */
    id: string
}

export interface TemplatesApiListStarterTemplatesRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApilistStarterTemplates
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof TemplatesApilistStarterTemplates
     */
    limit?: number
}

export interface TemplatesApiListTemplatesRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApilistTemplates
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof TemplatesApilistTemplates
     */
    limit?: number
}

export interface TemplatesApiUpdateTemplateRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof TemplatesApiupdateTemplate
     */
    id: string
    /**
     *
     * @type UpdateTemplateRequest
     * @memberof TemplatesApiupdateTemplate
     */
    updateTemplateRequest: UpdateTemplateRequest
}

export interface TemplatesApiValidateTemplateRequest {
    /**
     *
     * @type ValidateTemplateRequest
     * @memberof TemplatesApivalidateTemplate
     */
    validateTemplateRequest: ValidateTemplateRequest
}

export class ObjectTemplatesApi {
    private api: ObservableTemplatesApi

    public constructor(configuration: Configuration, requestFactory?: TemplatesApiRequestFactory, responseProcessor?: TemplatesApiResponseProcessor) {
        this.api = new ObservableTemplatesApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Create a reusable email template. subject and text (and html when present) must parse: {{variable}} interpolation with dot paths; {{{variable}}} renders raw in the HTML part. Alternatively set from_starter to copy a starter template verbatim. Beta: templates are unstable — their shape may change before they are declared stable.
     * Create a template (beta)
     * @param param the request object
     */
    public createTemplateWithHttpInfo(param: TemplatesApiCreateTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<TemplateView>> {
        return this.api.createTemplateWithHttpInfo(param.createTemplateRequest,  options).toPromise();
    }

    /**
     * Create a reusable email template. subject and text (and html when present) must parse: {{variable}} interpolation with dot paths; {{{variable}}} renders raw in the HTML part. Alternatively set from_starter to copy a starter template verbatim. Beta: templates are unstable — their shape may change before they are declared stable.
     * Create a template (beta)
     * @param param the request object
     */
    public createTemplate(param: TemplatesApiCreateTemplateRequest, options?: ConfigurationOptions): Promise<TemplateView> {
        return this.api.createTemplate(param.createTemplateRequest,  options).toPromise();
    }

    /**
     * Delete a template. In-flight sends are unaffected (rendering happens at send time). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}). Beta: templates are unstable — their shape may change before they are declared stable.
     * Delete a template (beta)
     * @param param the request object
     */
    public deleteTemplateWithHttpInfo(param: TemplatesApiDeleteTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteTemplateResult>> {
        return this.api.deleteTemplateWithHttpInfo(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Delete a template. In-flight sends are unaffected (rendering happens at send time). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}). Beta: templates are unstable — their shape may change before they are declared stable.
     * Delete a template (beta)
     * @param param the request object
     */
    public deleteTemplate(param: TemplatesApiDeleteTemplateRequest, options?: ConfigurationOptions): Promise<DeleteTemplateResult> {
        return this.api.deleteTemplate(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Fetch one starter template by alias, including its full plain-text and HTML body sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a starter template (beta)
     * @param param the request object
     */
    public getStarterTemplateWithHttpInfo(param: TemplatesApiGetStarterTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<StarterTemplateDetailView>> {
        return this.api.getStarterTemplateWithHttpInfo(param.alias,  options).toPromise();
    }

    /**
     * Fetch one starter template by alias, including its full plain-text and HTML body sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a starter template (beta)
     * @param param the request object
     */
    public getStarterTemplate(param: TemplatesApiGetStarterTemplateRequest, options?: ConfigurationOptions): Promise<StarterTemplateDetailView> {
        return this.api.getStarterTemplate(param.alias,  options).toPromise();
    }

    /**
     * Fetch one template by id. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a template (beta)
     * @param param the request object
     */
    public getTemplateWithHttpInfo(param: TemplatesApiGetTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<TemplateView>> {
        return this.api.getTemplateWithHttpInfo(param.id,  options).toPromise();
    }

    /**
     * Fetch one template by id. Beta: templates are unstable — their shape may change before they are declared stable.
     * Get a template (beta)
     * @param param the request object
     */
    public getTemplate(param: TemplatesApiGetTemplateRequest, options?: ConfigurationOptions): Promise<TemplateView> {
        return this.api.getTemplate(param.id,  options).toPromise();
    }

    /**
     * List the pre-built starter templates shipped with the deployment, sorted by alias. Returns catalog metadata only; fetch one by alias for the full body sources, or copy one into your library with from_starter on POST /v1/templates. Beta: templates are unstable — their shape may change before they are declared stable.
     * List starter templates (beta)
     * @param param the request object
     */
    public listStarterTemplatesWithHttpInfo(param: TemplatesApiListStarterTemplatesRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageStarterTemplateView>> {
        return this.api.listStarterTemplatesWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the pre-built starter templates shipped with the deployment, sorted by alias. Returns catalog metadata only; fetch one by alias for the full body sources, or copy one into your library with from_starter on POST /v1/templates. Beta: templates are unstable — their shape may change before they are declared stable.
     * List starter templates (beta)
     * @param param the request object
     */
    public listStarterTemplates(param: TemplatesApiListStarterTemplatesRequest = {}, options?: ConfigurationOptions): Promise<PageStarterTemplateView> {
        return this.api.listStarterTemplates(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the account\'s templates, newest first. Returns metadata only (no text/html); fetch one by id for the full sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * List templates (beta)
     * @param param the request object
     */
    public listTemplatesWithHttpInfo(param: TemplatesApiListTemplatesRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageTemplateSummaryView>> {
        return this.api.listTemplatesWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the account\'s templates, newest first. Returns metadata only (no text/html); fetch one by id for the full sources. Beta: templates are unstable — their shape may change before they are declared stable.
     * List templates (beta)
     * @param param the request object
     */
    public listTemplates(param: TemplatesApiListTemplatesRequest = {}, options?: ConfigurationOptions): Promise<PageTemplateSummaryView> {
        return this.api.listTemplates(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Partial update. Changed template parts are re-parsed; set alias or html to \"\" to clear them. Beta: templates are unstable — their shape may change before they are declared stable.
     * Update a template (beta)
     * @param param the request object
     */
    public updateTemplateWithHttpInfo(param: TemplatesApiUpdateTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<TemplateView>> {
        return this.api.updateTemplateWithHttpInfo(param.id, param.updateTemplateRequest,  options).toPromise();
    }

    /**
     * Partial update. Changed template parts are re-parsed; set alias or html to \"\" to clear them. Beta: templates are unstable — their shape may change before they are declared stable.
     * Update a template (beta)
     * @param param the request object
     */
    public updateTemplate(param: TemplatesApiUpdateTemplateRequest, options?: ConfigurationOptions): Promise<TemplateView> {
        return this.api.updateTemplate(param.id, param.updateTemplateRequest,  options).toPromise();
    }

    /**
     * Dry-run template source without persisting: reports per-part parse errors, a rendered preview (against test_data when provided), and suggested_data — a placeholder value for every variable the source references. Beta: templates are unstable — their shape may change before they are declared stable.
     * Validate template source (beta)
     * @param param the request object
     */
    public validateTemplateWithHttpInfo(param: TemplatesApiValidateTemplateRequest, options?: ConfigurationOptions): Promise<HttpInfo<ValidateTemplateResponse>> {
        return this.api.validateTemplateWithHttpInfo(param.validateTemplateRequest,  options).toPromise();
    }

    /**
     * Dry-run template source without persisting: reports per-part parse errors, a rendered preview (against test_data when provided), and suggested_data — a placeholder value for every variable the source references. Beta: templates are unstable — their shape may change before they are declared stable.
     * Validate template source (beta)
     * @param param the request object
     */
    public validateTemplate(param: TemplatesApiValidateTemplateRequest, options?: ConfigurationOptions): Promise<ValidateTemplateResponse> {
        return this.api.validateTemplate(param.validateTemplateRequest,  options).toPromise();
    }

}

import { ObservableWebhooksApi } from "./ObservableAPI.js";
import { WebhooksApiRequestFactory, WebhooksApiResponseProcessor} from "../apis/WebhooksApi.js";

export interface WebhooksApiCreateWebhookRequest {
    /**
     *
     * @type CreateWebhookRequest
     * @memberof WebhooksApicreateWebhook
     */
    createWebhookRequest: CreateWebhookRequest
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response — the SAME webhook id and one-time signing secret — instead of registering a second active subscription. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). A keyed create commits the webhook and its replay response atomically, so an accepted create always replays; dedup is best-effort only under idempotency-store degradation before that commit.
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApicreateWebhook
     */
    idempotencyKey?: string
}

export interface WebhooksApiDeleteWebhookRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApideleteWebhook
     */
    id: string
    /**
     * Must be the literal DELETE — this action is irreversible.
     * Defaults to: undefined
     * @type &#39;DELETE&#39;
     * @memberof WebhooksApideleteWebhook
     */
    confirm: 'DELETE'
}

export interface WebhooksApiGetWebhookRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApigetWebhook
     */
    id: string
}

export interface WebhooksApiListWebhookDeliveriesRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApilistWebhookDeliveries
     */
    id: string
    /**
     *
     * Defaults to: undefined
     * @type &#39;pending&#39; | &#39;delivered&#39; | &#39;failed&#39;
     * @memberof WebhooksApilistWebhookDeliveries
     */
    status?: 'pending' | 'delivered' | 'failed'
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the status filter.
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApilistWebhookDeliveries
     */
    cursor?: string
    /**
     *
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof WebhooksApilistWebhookDeliveries
     */
    limit?: number
}

export interface WebhooksApiListWebhooksRequest {
    /**
     * Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApilistWebhooks
     */
    cursor?: string
    /**
     * Maximum number of items to return (1-100).
     * Minimum: 1
     * Maximum: 100
     * Defaults to: 100
     * @type number
     * @memberof WebhooksApilistWebhooks
     */
    limit?: number
}

export interface WebhooksApiRotateWebhookSecretRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApirotateWebhookSecret
     */
    id: string
    /**
     * Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first request\&#39;s response instead of re-executing it. Completed keys are remembered for at least 24 hours (the published minimum dedup window). Within the window: same key + different body → 422 idempotency_key_reuse (do not retry as-is); same key while the first request is still executing → 409 idempotency_in_flight (wait, then retry unchanged). Dedup is best-effort: under idempotency-store degradation or a mid-request crash the guarantee degrades to at-least-once — a keyed retry may re-execute rather than replay.
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApirotateWebhookSecret
     */
    idempotencyKey?: string
}

export interface WebhooksApiTestWebhookRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApitestWebhook
     */
    id: string
    /**
     *
     * @type TestWebhookRequest
     * @memberof WebhooksApitestWebhook
     */
    testWebhookRequest: TestWebhookRequest
}

export interface WebhooksApiUpdateWebhookRequest {
    /**
     *
     * Defaults to: undefined
     * @type string
     * @memberof WebhooksApiupdateWebhook
     */
    id: string
    /**
     *
     * @type UpdateWebhookRequest
     * @memberof WebhooksApiupdateWebhook
     */
    updateWebhookRequest: UpdateWebhookRequest
}

export class ObjectWebhooksApi {
    private api: ObservableWebhooksApi

    public constructor(configuration: Configuration, requestFactory?: WebhooksApiRequestFactory, responseProcessor?: WebhooksApiResponseProcessor) {
        this.api = new ObservableWebhooksApi(configuration, requestFactory, responseProcessor);
    }

    /**
     * Register a webhook subscriber; the one-time signing secret is returned only on this response. Honors Idempotency-Key so a retried create replays the same webhook (same id + secret) instead of registering a second subscription; omit the key to intentionally create distinct subscriptions, including several to the same URL.
     * Create a webhook
     * @param param the request object
     */
    public createWebhookWithHttpInfo(param: WebhooksApiCreateWebhookRequest, options?: ConfigurationOptions): Promise<HttpInfo<CreateWebhookResponse>> {
        return this.api.createWebhookWithHttpInfo(param.createWebhookRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Register a webhook subscriber; the one-time signing secret is returned only on this response. Honors Idempotency-Key so a retried create replays the same webhook (same id + secret) instead of registering a second subscription; omit the key to intentionally create distinct subscriptions, including several to the same URL.
     * Create a webhook
     * @param param the request object
     */
    public createWebhook(param: WebhooksApiCreateWebhookRequest, options?: ConfigurationOptions): Promise<CreateWebhookResponse> {
        return this.api.createWebhook(param.createWebhookRequest, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Delete a webhook subscriber by id. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Delete a webhook
     * @param param the request object
     */
    public deleteWebhookWithHttpInfo(param: WebhooksApiDeleteWebhookRequest, options?: ConfigurationOptions): Promise<HttpInfo<DeleteWebhookResult>> {
        return this.api.deleteWebhookWithHttpInfo(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Delete a webhook subscriber by id. Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}).
     * Delete a webhook
     * @param param the request object
     */
    public deleteWebhook(param: WebhooksApiDeleteWebhookRequest, options?: ConfigurationOptions): Promise<DeleteWebhookResult> {
        return this.api.deleteWebhook(param.id, param.confirm,  options).toPromise();
    }

    /**
     * Get a webhook
     * @param param the request object
     */
    public getWebhookWithHttpInfo(param: WebhooksApiGetWebhookRequest, options?: ConfigurationOptions): Promise<HttpInfo<WebhookView>> {
        return this.api.getWebhookWithHttpInfo(param.id,  options).toPromise();
    }

    /**
     * Get a webhook
     * @param param the request object
     */
    public getWebhook(param: WebhooksApiGetWebhookRequest, options?: ConfigurationOptions): Promise<WebhookView> {
        return this.api.getWebhook(param.id,  options).toPromise();
    }

    /**
     * The per-webhook delivery log (read-only debug view).
     * List webhook deliveries
     * @param param the request object
     */
    public listWebhookDeliveriesWithHttpInfo(param: WebhooksApiListWebhookDeliveriesRequest, options?: ConfigurationOptions): Promise<HttpInfo<PageWebhookDeliveryView>> {
        return this.api.listWebhookDeliveriesWithHttpInfo(param.id, param.status, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * The per-webhook delivery log (read-only debug view).
     * List webhook deliveries
     * @param param the request object
     */
    public listWebhookDeliveries(param: WebhooksApiListWebhookDeliveriesRequest, options?: ConfigurationOptions): Promise<PageWebhookDeliveryView> {
        return this.api.listWebhookDeliveries(param.id, param.status, param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the webhooks owned by the authenticated account, newest first, with cursor pagination.
     * List webhooks
     * @param param the request object
     */
    public listWebhooksWithHttpInfo(param: WebhooksApiListWebhooksRequest = {}, options?: ConfigurationOptions): Promise<HttpInfo<PageWebhookView>> {
        return this.api.listWebhooksWithHttpInfo(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * List the webhooks owned by the authenticated account, newest first, with cursor pagination.
     * List webhooks
     * @param param the request object
     */
    public listWebhooks(param: WebhooksApiListWebhooksRequest = {}, options?: ConfigurationOptions): Promise<PageWebhookView> {
        return this.api.listWebhooks(param.cursor, param.limit,  options).toPromise();
    }

    /**
     * Mint a new signing secret; the previous one stays valid for a 24h grace window. Returns the new secret (shown once). Honors Idempotency-Key so a retried rotate replays the same secret instead of rotating twice (rotate has no request body, so the dedup hash covers the route alone — the same key on a different webhook id is a 422 idempotency_key_reuse).
     * Rotate a webhook signing secret
     * @param param the request object
     */
    public rotateWebhookSecretWithHttpInfo(param: WebhooksApiRotateWebhookSecretRequest, options?: ConfigurationOptions): Promise<HttpInfo<RotateSecretResponse>> {
        return this.api.rotateWebhookSecretWithHttpInfo(param.id, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Mint a new signing secret; the previous one stays valid for a 24h grace window. Returns the new secret (shown once). Honors Idempotency-Key so a retried rotate replays the same secret instead of rotating twice (rotate has no request body, so the dedup hash covers the route alone — the same key on a different webhook id is a 422 idempotency_key_reuse).
     * Rotate a webhook signing secret
     * @param param the request object
     */
    public rotateWebhookSecret(param: WebhooksApiRotateWebhookSecretRequest, options?: ConfigurationOptions): Promise<RotateSecretResponse> {
        return this.api.rotateWebhookSecret(param.id, param.idempotencyKey,  options).toPromise();
    }

    /**
     * Schedule a one-off synthetic delivery to this webhook for development. Returns the delivery id.
     * Fire a synthetic event
     * @param param the request object
     */
    public testWebhookWithHttpInfo(param: WebhooksApiTestWebhookRequest, options?: ConfigurationOptions): Promise<HttpInfo<TestWebhookResponse>> {
        return this.api.testWebhookWithHttpInfo(param.id, param.testWebhookRequest,  options).toPromise();
    }

    /**
     * Schedule a one-off synthetic delivery to this webhook for development. Returns the delivery id.
     * Fire a synthetic event
     * @param param the request object
     */
    public testWebhook(param: WebhooksApiTestWebhookRequest, options?: ConfigurationOptions): Promise<TestWebhookResponse> {
        return this.api.testWebhook(param.id, param.testWebhookRequest,  options).toPromise();
    }

    /**
     * Partial update. url/events/filters are full-replace when present. Re-enabling within the auto-disable cooldown returns 409.
     * Update a webhook
     * @param param the request object
     */
    public updateWebhookWithHttpInfo(param: WebhooksApiUpdateWebhookRequest, options?: ConfigurationOptions): Promise<HttpInfo<WebhookView>> {
        return this.api.updateWebhookWithHttpInfo(param.id, param.updateWebhookRequest,  options).toPromise();
    }

    /**
     * Partial update. url/events/filters are full-replace when present. Re-enabling within the auto-disable cooldown returns 409.
     * Update a webhook
     * @param param the request object
     */
    public updateWebhook(param: WebhooksApiUpdateWebhookRequest, options?: ConfigurationOptions): Promise<WebhookView> {
        return this.api.updateWebhook(param.id, param.updateWebhookRequest,  options).toPromise();
    }

}
