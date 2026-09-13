// TODO: better import syntax?
import {BaseAPIRequestFactory, RequiredError, COLLECTION_FORMATS} from './baseapi.js';
import {Configuration} from '../configuration.js';
import {RequestContext, HttpMethod, ResponseContext, HttpFile, HttpInfo} from '../http/http.js';
import {ObjectSerializer} from '../models/ObjectSerializer.js';
import {ApiException} from './exception.js';
import {canConsumeForm, isCodeInRange} from '../util.js';
import {SecurityAuthentication} from '../auth/auth.js';


import { AgentSignupCreateResponse } from '../models/AgentSignupCreateResponse.js';
import { AgentSignupRequest } from '../models/AgentSignupRequest.js';
import { AgentSignupView } from '../models/AgentSignupView.js';
import { ApproveAgentSignupInputBody } from '../models/ApproveAgentSignupInputBody.js';
import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { PageAgentSignupView } from '../models/PageAgentSignupView.js';
import { RateLimitedEnvelope } from '../models/RateLimitedEnvelope.js';
import { RejectAgentSignupOutputBody } from '../models/RejectAgentSignupOutputBody.js';
import { VerifyAgentSignupRequest } from '../models/VerifyAgentSignupRequest.js';

/**
 * no description
 */
export class AgentSignupApiRequestFactory extends BaseAPIRequestFactory {

    /**
     * Account-scoped. Optionally routes the verified agent\'s non-human outbound through the existing review queue.
     * Approve a pending agent identity (beta)
     * @param id
     * @param approveAgentSignupInputBody
     */
    public async approveAgentSignup(id: string, approveAgentSignupInputBody: ApproveAgentSignupInputBody, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'id' is not null or undefined
        if (id === null || id === undefined) {
            throw new RequiredError("AgentSignupApi", "approveAgentSignup", "id");
        }


        // verify required parameter 'approveAgentSignupInputBody' is not null or undefined
        if (approveAgentSignupInputBody === null || approveAgentSignupInputBody === undefined) {
            throw new RequiredError("AgentSignupApi", "approveAgentSignup", "approveAgentSignupInputBody");
        }


        // Path Params
        const localVarPath = '/v1/agent-signup/{id}/approve'
            .replace('{' + 'id' + '}', encodeURIComponent(String(id)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(approveAgentSignupInputBody, "ApproveAgentSignupInputBody", ""),
            contentType
        );
        requestContext.setBody(serializedBody);

        let authMethod: SecurityAuthentication | undefined;
        // Apply auth methods
        authMethod = _config.authMethods["bearer"]
        if (authMethod?.applySecurityAuthentication) {
            await authMethod?.applySecurityAuthentication(requestContext);
        }

        const defaultAuth: SecurityAuthentication | undefined = _config?.authMethods?.default
        if (defaultAuth?.applySecurityAuthentication) {
            await defaultAuth?.applySecurityAuthentication(requestContext);
        }

        return requestContext;
    }

    /**
     * Public, no API key required for first signup. Creates one receiving inbox and an agent-scoped key, then sends a six-digit code to the human. Repeating the same human_email + display_name requires current_api_key, rotates that key, and resends verification.
     * Create a provisional agent identity (beta)
     * @param agentSignupRequest
     */
    public async createAgentSignup(agentSignupRequest: AgentSignupRequest, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'agentSignupRequest' is not null or undefined
        if (agentSignupRequest === null || agentSignupRequest === undefined) {
            throw new RequiredError("AgentSignupApi", "createAgentSignup", "agentSignupRequest");
        }


        // Path Params
        const localVarPath = '/v1/agent-signup';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(agentSignupRequest, "AgentSignupRequest", ""),
            contentType
        );
        requestContext.setBody(serializedBody);


        const defaultAuth: SecurityAuthentication | undefined = _config?.authMethods?.default
        if (defaultAuth?.applySecurityAuthentication) {
            await defaultAuth?.applySecurityAuthentication(requestContext);
        }

        return requestContext;
    }

    /**
     * List pending agent identities for the signed-in human (beta)
     * @param cursor Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param limit Maximum number of items to return (1-100).
     */
    public async listPendingAgentSignups(cursor?: string, limit?: number, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;



        // Path Params
        const localVarPath = '/v1/agent-signup/pending';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.GET);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (cursor !== undefined) {
            requestContext.setQueryParam("cursor", ObjectSerializer.serialize(cursor, "string", ""));
        }

        // Query Params
        if (limit !== undefined) {
            requestContext.setQueryParam("limit", ObjectSerializer.serialize(limit, "number", "int64"));
        }


        let authMethod: SecurityAuthentication | undefined;
        // Apply auth methods
        authMethod = _config.authMethods["bearer"]
        if (authMethod?.applySecurityAuthentication) {
            await authMethod?.applySecurityAuthentication(requestContext);
        }

        const defaultAuth: SecurityAuthentication | undefined = _config?.authMethods?.default
        if (defaultAuth?.applySecurityAuthentication) {
            await defaultAuth?.applySecurityAuthentication(requestContext);
        }

        return requestContext;
    }

    /**
     * Account-scoped. Revokes the signup key and deactivates the inbox.
     * Reject and deactivate a pending agent identity (beta)
     * @param id
     * @param requestBody
     */
    public async rejectAgentSignup(id: string, requestBody: { [key: string]: any; }, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'id' is not null or undefined
        if (id === null || id === undefined) {
            throw new RequiredError("AgentSignupApi", "rejectAgentSignup", "id");
        }


        // verify required parameter 'requestBody' is not null or undefined
        if (requestBody === null || requestBody === undefined) {
            throw new RequiredError("AgentSignupApi", "rejectAgentSignup", "requestBody");
        }


        // Path Params
        const localVarPath = '/v1/agent-signup/{id}/reject'
            .replace('{' + 'id' + '}', encodeURIComponent(String(id)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(requestBody, "{ [key: string]: any; }", ""),
            contentType
        );
        requestContext.setBody(serializedBody);

        let authMethod: SecurityAuthentication | undefined;
        // Apply auth methods
        authMethod = _config.authMethods["bearer"]
        if (authMethod?.applySecurityAuthentication) {
            await authMethod?.applySecurityAuthentication(requestContext);
        }

        const defaultAuth: SecurityAuthentication | undefined = _config?.authMethods?.default
        if (defaultAuth?.applySecurityAuthentication) {
            await defaultAuth?.applySecurityAuthentication(requestContext);
        }

        return requestContext;
    }

    /**
     * Requires the agent-scoped key returned by signup. A successful code verification unlocks normal plan limits and may enable the existing human-review queue.
     * Verify a provisional identity with its code (beta)
     * @param verifyAgentSignupRequest
     */
    public async verifyAgentSignup(verifyAgentSignupRequest: VerifyAgentSignupRequest, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'verifyAgentSignupRequest' is not null or undefined
        if (verifyAgentSignupRequest === null || verifyAgentSignupRequest === undefined) {
            throw new RequiredError("AgentSignupApi", "verifyAgentSignup", "verifyAgentSignupRequest");
        }


        // Path Params
        const localVarPath = '/v1/agent-signup/verify';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(verifyAgentSignupRequest, "VerifyAgentSignupRequest", ""),
            contentType
        );
        requestContext.setBody(serializedBody);

        let authMethod: SecurityAuthentication | undefined;
        // Apply auth methods
        authMethod = _config.authMethods["bearer"]
        if (authMethod?.applySecurityAuthentication) {
            await authMethod?.applySecurityAuthentication(requestContext);
        }

        const defaultAuth: SecurityAuthentication | undefined = _config?.authMethods?.default
        if (defaultAuth?.applySecurityAuthentication) {
            await defaultAuth?.applySecurityAuthentication(requestContext);
        }

        return requestContext;
    }

}

export class AgentSignupApiResponseProcessor {

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to approveAgentSignup
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async approveAgentSignupWithHttpInfo(response: ResponseContext): Promise<HttpInfo<AgentSignupView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: AgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupView", ""
            ) as AgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("0", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Error", body, response.headers);
        }

        // Work around for missing responses in specification, e.g. for petstore.yaml
        if (response.httpStatusCode >= 200 && response.httpStatusCode <= 299) {
            const body: AgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupView", ""
            ) as AgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to createAgentSignup
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async createAgentSignupWithHttpInfo(response: ResponseContext): Promise<HttpInfo<AgentSignupCreateResponse >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: AgentSignupCreateResponse = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupCreateResponse", ""
            ) as AgentSignupCreateResponse;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("201", response.httpStatusCode)) {
            const body: AgentSignupCreateResponse = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupCreateResponse", ""
            ) as AgentSignupCreateResponse;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("429", response.httpStatusCode)) {
            const body: RateLimitedEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "RateLimitedEnvelope", ""
            ) as RateLimitedEnvelope;
            throw new ApiException<RateLimitedEnvelope>(response.httpStatusCode, "Too Many Requests — a request-RATE / throughput limit was hit (code rate_limited). This is distinct from a 402 limit_exceeded (a QUOTA cap): a 429 is transient and retry-able — wait error.details.retry_after_seconds (mirrored on the Retry-After header), then the same request succeeds. Branch on the HTTP status: 429 → back off and retry; 402 → surface a quota/upgrade path.", body, response.headers);
        }
        if (isCodeInRange("0", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Error — the standard envelope; branch on error.code.", body, response.headers);
        }

        // Work around for missing responses in specification, e.g. for petstore.yaml
        if (response.httpStatusCode >= 200 && response.httpStatusCode <= 299) {
            const body: AgentSignupCreateResponse = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupCreateResponse", ""
            ) as AgentSignupCreateResponse;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to listPendingAgentSignups
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async listPendingAgentSignupsWithHttpInfo(response: ResponseContext): Promise<HttpInfo<PageAgentSignupView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: PageAgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageAgentSignupView", ""
            ) as PageAgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("0", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Error", body, response.headers);
        }

        // Work around for missing responses in specification, e.g. for petstore.yaml
        if (response.httpStatusCode >= 200 && response.httpStatusCode <= 299) {
            const body: PageAgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageAgentSignupView", ""
            ) as PageAgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to rejectAgentSignup
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async rejectAgentSignupWithHttpInfo(response: ResponseContext): Promise<HttpInfo<RejectAgentSignupOutputBody >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: RejectAgentSignupOutputBody = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "RejectAgentSignupOutputBody", ""
            ) as RejectAgentSignupOutputBody;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("0", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Error", body, response.headers);
        }

        // Work around for missing responses in specification, e.g. for petstore.yaml
        if (response.httpStatusCode >= 200 && response.httpStatusCode <= 299) {
            const body: RejectAgentSignupOutputBody = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "RejectAgentSignupOutputBody", ""
            ) as RejectAgentSignupOutputBody;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to verifyAgentSignup
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async verifyAgentSignupWithHttpInfo(response: ResponseContext): Promise<HttpInfo<AgentSignupView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: AgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupView", ""
            ) as AgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("0", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Error", body, response.headers);
        }

        // Work around for missing responses in specification, e.g. for petstore.yaml
        if (response.httpStatusCode >= 200 && response.httpStatusCode <= 299) {
            const body: AgentSignupView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "AgentSignupView", ""
            ) as AgentSignupView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

}
