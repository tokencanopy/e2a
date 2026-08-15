// TODO: better import syntax?
import {BaseAPIRequestFactory, RequiredError, COLLECTION_FORMATS} from './baseapi.js';
import {Configuration} from '../configuration.js';
import {RequestContext, HttpMethod, ResponseContext, HttpFile, HttpInfo} from '../http/http.js';
import {ObjectSerializer} from '../models/ObjectSerializer.js';
import {ApiException} from './exception.js';
import {canConsumeForm, isCodeInRange} from '../util.js';
import {SecurityAuthentication} from '../auth/auth.js';


import { DeleteDomainResult } from '../models/DeleteDomainResult.js';
import { DomainView } from '../models/DomainView.js';
import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { LimitExceededEnvelope } from '../models/LimitExceededEnvelope.js';
import { PageDomainView } from '../models/PageDomainView.js';
import { RegisterDomainRequest } from '../models/RegisterDomainRequest.js';
import { VerifyDomainView } from '../models/VerifyDomainView.js';

/**
 * no description
 */
export class DomainsApiRequestFactory extends BaseAPIRequestFactory {

    /**
     * Deletes the domain (refused with 400 domain_has_agents while any live or trashed agent exists on it) and commits durable teardown of its sending identity. The provider-side identity is normally removed before the response returns; otherwise sending_teardown is pending (durable retries, including provider-disabled managed identities) or manual_review (an identity exists but ownership cannot be established). Send a unique Idempotency-Key for each logical deletion and reuse that key after an ambiguous network failure: the original receipt is replayed without deleting a later registration of the same domain. Use a new key to delete a replacement registration. Without a key, repeating DELETE only polls while the domain remains absent and is unsafe across re-registration. Keep DNS published unless sending_teardown is confirmed; treat missing or unknown values as not confirmed. Requires ?confirm=DELETE (irreversible). Returns 200 with a deletion object ({deleted:true, domain, sending_teardown}).
     * Delete a domain
     * @param domain 
     * @param confirm Must be the literal DELETE — this action is irreversible.
     * @param idempotencyKey Optional idempotency key for safe retries (unique per logical domain deletion). A retry with the same key replays the first deletion receipt instead of deleting a replacement registration of the same domain. Completed keys are remembered for at least 24 hours. Reuse the original key after an ambiguous failure; use a new key only for a new domain incarnation. Same key on a different domain returns 422 idempotency_key_reuse; a concurrent request with the same key returns 409 idempotency_in_flight.
     */
    public async deleteDomain(domain: string, confirm: 'DELETE', idempotencyKey?: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'domain' is not null or undefined
        if (domain === null || domain === undefined) {
            throw new RequiredError("DomainsApi", "deleteDomain", "domain");
        }


        // verify required parameter 'confirm' is not null or undefined
        if (confirm === null || confirm === undefined) {
            throw new RequiredError("DomainsApi", "deleteDomain", "confirm");
        }



        // Path Params
        const localVarPath = '/v1/domains/{domain}'
            .replace('{' + 'domain' + '}', encodeURIComponent(String(domain)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.DELETE);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (confirm !== undefined) {
            requestContext.setQueryParam("confirm", ObjectSerializer.serialize(confirm, "'DELETE'", ""));
        }

        // Header Params
        requestContext.setHeaderParam("Idempotency-Key", ObjectSerializer.serialize(idempotencyKey, "string", ""));


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
     * Get a domain
     * @param domain 
     */
    public async getDomain(domain: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'domain' is not null or undefined
        if (domain === null || domain === undefined) {
            throw new RequiredError("DomainsApi", "getDomain", "domain");
        }


        // Path Params
        const localVarPath = '/v1/domains/{domain}'
            .replace('{' + 'domain' + '}', encodeURIComponent(String(domain)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.GET);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


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
     * List the domains owned by the authenticated account, newest first, with cursor pagination.
     * List domains
     * @param cursor Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param limit Maximum number of items to return (1-100).
     */
    public async listDomains(cursor?: string, limit?: number, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;



        // Path Params
        const localVarPath = '/v1/domains';

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
     * Register a domain
     * @param registerDomainRequest 
     */
    public async registerDomain(registerDomainRequest: RegisterDomainRequest, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'registerDomainRequest' is not null or undefined
        if (registerDomainRequest === null || registerDomainRequest === undefined) {
            throw new RequiredError("DomainsApi", "registerDomain", "registerDomainRequest");
        }


        // Path Params
        const localVarPath = '/v1/domains';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(registerDomainRequest, "RegisterDomainRequest", ""),
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
     * Probe the domain\'s published DNS and, when the verification TXT (and inbound MX) are present, mark it verified. Always returns 200 with the per-record diagnostic — branch on the `verified` boolean in the body, not the HTTP status. A not-yet-published record is the normal `verified:false` outcome, not an error.
     * Verify a domain
     * @param domain 
     */
    public async verifyDomain(domain: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'domain' is not null or undefined
        if (domain === null || domain === undefined) {
            throw new RequiredError("DomainsApi", "verifyDomain", "domain");
        }


        // Path Params
        const localVarPath = '/v1/domains/{domain}/verify'
            .replace('{' + 'domain' + '}', encodeURIComponent(String(domain)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")


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

export class DomainsApiResponseProcessor {

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to deleteDomain
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async deleteDomainWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DeleteDomainResult >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: DeleteDomainResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteDomainResult", ""
            ) as DeleteDomainResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("409", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Conflict — code idempotency_in_flight: a request with this Idempotency-Key is still executing. Retry-able: wait for the first request to finish, then retry with the SAME key and byte-identical body — the retry replays the first request\&#39;s response instead of re-executing the side effect.", body, response.headers);
        }
        if (isCodeInRange("422", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Unprocessable — branch on error.code. idempotency_key_reuse: this Idempotency-Key was already used with a DIFFERENT request body (the dedup hash covers the route + the raw body bytes) — do NOT retry as-is; a legitimate retry must resend the byte-identical body, and a genuinely new request needs a fresh key. invalid_request: a semantic validation failure in the request body.", body, response.headers);
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
            const body: DeleteDomainResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteDomainResult", ""
            ) as DeleteDomainResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to getDomain
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async getDomainWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DomainView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: DomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DomainView", ""
            ) as DomainView;
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
            const body: DomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DomainView", ""
            ) as DomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to listDomains
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async listDomainsWithHttpInfo(response: ResponseContext): Promise<HttpInfo<PageDomainView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: PageDomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageDomainView", ""
            ) as PageDomainView;
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
            const body: PageDomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageDomainView", ""
            ) as PageDomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to registerDomain
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async registerDomainWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DomainView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("201", response.httpStatusCode)) {
            const body: DomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DomainView", ""
            ) as DomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("402", response.httpStatusCode)) {
            const body: LimitExceededEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "LimitExceededEnvelope", ""
            ) as LimitExceededEnvelope;
            throw new ApiException<LimitExceededEnvelope>(response.httpStatusCode, "Payment required — a per-account resource cap was hit (code limit_exceeded). error.details.resource is the AccountView usage/limits field stem (agents, domains, messages_month, storage_bytes), so the client can key it to usage.&lt;resource&gt; / limits.max_&lt;resource&gt;. This is a QUOTA (stock/flow) cap — distinct from a 429 rate_limited (throughput). A retry alone will not clear it; surface a quota/upgrade path.", body, response.headers);
        }
        if (isCodeInRange("409", response.httpStatusCode)) {
            const body: ErrorEnvelope = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ErrorEnvelope", ""
            ) as ErrorEnvelope;
            throw new ApiException<ErrorEnvelope>(response.httpStatusCode, "Conflict — the domain is already claimed by another account (code domain_taken).", body, response.headers);
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
            const body: DomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DomainView", ""
            ) as DomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to verifyDomain
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async verifyDomainWithHttpInfo(response: ResponseContext): Promise<HttpInfo<VerifyDomainView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: VerifyDomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "VerifyDomainView", ""
            ) as VerifyDomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
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
            const body: VerifyDomainView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "VerifyDomainView", ""
            ) as VerifyDomainView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

}
