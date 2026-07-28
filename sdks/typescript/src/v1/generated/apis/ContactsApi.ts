// TODO: better import syntax?
import {BaseAPIRequestFactory, RequiredError, COLLECTION_FORMATS} from './baseapi.js';
import {Configuration} from '../configuration.js';
import {RequestContext, HttpMethod, ResponseContext, HttpFile, HttpInfo} from '../http/http.js';
import {ObjectSerializer} from '../models/ObjectSerializer.js';
import {ApiException} from './exception.js';
import {canConsumeForm, isCodeInRange} from '../util.js';
import {SecurityAuthentication} from '../auth/auth.js';


import { ContactEngagementView } from '../models/ContactEngagementView.js';
import { ContactImportResult } from '../models/ContactImportResult.js';
import { ContactView } from '../models/ContactView.js';
import { CreateContactRequest } from '../models/CreateContactRequest.js';
import { DeleteContactResult } from '../models/DeleteContactResult.js';
import { DeleteEngagementResult } from '../models/DeleteEngagementResult.js';
import { DeleteImportBatchResult } from '../models/DeleteImportBatchResult.js';
import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { ImportContactsRequest } from '../models/ImportContactsRequest.js';
import { PageContactEngagementView } from '../models/PageContactEngagementView.js';
import { PageContactView } from '../models/PageContactView.js';
import { UpdateContactRequest } from '../models/UpdateContactRequest.js';
import { UpsertEngagementRequest } from '../models/UpsertEngagementRequest.js';

/**
 * no description
 */
export class ContactsApiRequestFactory extends BaseAPIRequestFactory {

    /**
     * Creates one contact. The address is canonicalized before storage, so a display-name form and the bare address are the same contact — a second create returns 409. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Create a contact (beta)
     * @param createContactRequest 
     * @param idempotencyKey Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first response instead of creating a second contact. Within the dedup window: same key + different body → 422 idempotency_key_reuse; same key while the first request is still executing → 409 idempotency_in_flight.
     */
    public async createContact(createContactRequest: CreateContactRequest, idempotencyKey?: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'createContactRequest' is not null or undefined
        if (createContactRequest === null || createContactRequest === undefined) {
            throw new RequiredError("ContactsApi", "createContact", "createContactRequest");
        }



        // Path Params
        const localVarPath = '/v1/contacts';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Header Params
        requestContext.setHeaderParam("Idempotency-Key", ObjectSerializer.serialize(idempotencyKey, "string", ""));


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(createContactRequest, "CreateContactRequest", ""),
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
     * Removes a contact. Requires ?confirm=DELETE. Suppressions are NOT affected — consent outlives the contact record, so deleting a contact never makes a previously-blocked address sendable. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Delete a contact (beta)
     * @param address 
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public async deleteContact(address: string, confirm: 'DELETE', _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "deleteContact", "address");
        }


        // verify required parameter 'confirm' is not null or undefined
        if (confirm === null || confirm === undefined) {
            throw new RequiredError("ContactsApi", "deleteContact", "confirm");
        }


        // Path Params
        const localVarPath = '/v1/contacts/{address}'
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.DELETE);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (confirm !== undefined) {
            requestContext.setQueryParam("confirm", ObjectSerializer.serialize(confirm, "'DELETE'", ""));
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
     * Removes this agent\'s outreach state for a contact. Requires ?confirm=DELETE. The contact itself survives (identity is account-level and other agents may still be working them) and suppressions are untouched — un-enrolling is not consent and never restores sendability. Beta: the outreach surface may change before it is declared stable.
     * Un-enrol a contact (beta)
     * @param email 
     * @param address 
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public async deleteEngagement(email: string, address: string, confirm: 'DELETE', _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'email' is not null or undefined
        if (email === null || email === undefined) {
            throw new RequiredError("ContactsApi", "deleteEngagement", "email");
        }


        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "deleteEngagement", "address");
        }


        // verify required parameter 'confirm' is not null or undefined
        if (confirm === null || confirm === undefined) {
            throw new RequiredError("ContactsApi", "deleteEngagement", "confirm");
        }


        // Path Params
        const localVarPath = '/v1/agents/{email}/contacts/{address}'
            .replace('{' + 'email' + '}', encodeURIComponent(String(email)))
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.DELETE);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (confirm !== undefined) {
            requestContext.setQueryParam("confirm", ObjectSerializer.serialize(confirm, "'DELETE'", ""));
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
     * Reverses the durable import batch. Requires ?confirm=DELETE. It removes untouched contacts created by the batch and per-agent enrolments the batch created, including enrolments on pre-existing contacts. Contacts with correspondence history are retained; pre-existing outreach and suppressions are never affected. The response reports each category. Account-scoped credentials only. Beta: the contact import surface may change before it is declared stable.
     * Reverse a contact import (beta)
     * @param batchId 
     * @param confirm Must be the literal DELETE — this action is irreversible.
     */
    public async deleteImportBatch(batchId: string, confirm: 'DELETE', _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'batchId' is not null or undefined
        if (batchId === null || batchId === undefined) {
            throw new RequiredError("ContactsApi", "deleteImportBatch", "batchId");
        }


        // verify required parameter 'confirm' is not null or undefined
        if (confirm === null || confirm === undefined) {
            throw new RequiredError("ContactsApi", "deleteImportBatch", "confirm");
        }


        // Path Params
        const localVarPath = '/v1/contacts/imports/{batch_id}'
            .replace('{' + 'batch_id' + '}', encodeURIComponent(String(batchId)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.DELETE);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (confirm !== undefined) {
            requestContext.setQueryParam("confirm", ObjectSerializer.serialize(confirm, "'DELETE'", ""));
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
     * Fetches one contact by address. Returns an ETag for use with If-Match on a subsequent update. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Get a contact (beta)
     * @param address The contact\&#39;s email address, URL-encoded.
     */
    public async getContact(address: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "getContact", "address");
        }


        // Path Params
        const localVarPath = '/v1/contacts/{address}'
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

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
     * Fetches this agent\'s relationship with one contact. Returns an ETag for use with If-Match on a subsequent update. Agent-scoped credentials may read their own agent. Beta: the outreach surface may change before it is declared stable.
     * Get one outreach record (beta)
     * @param email 
     * @param address 
     */
    public async getEngagement(email: string, address: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'email' is not null or undefined
        if (email === null || email === undefined) {
            throw new RequiredError("ContactsApi", "getEngagement", "email");
        }


        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "getEngagement", "address");
        }


        // Path Params
        const localVarPath = '/v1/agents/{email}/contacts/{address}'
            .replace('{' + 'email' + '}', encodeURIComponent(String(email)))
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

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
     * Creates or updates up to 1000 contacts in one request and returns a per-row outcome, so one malformed row never rejects the rest of the upload. Import is inert: it records identity and sends nothing. Addresses already on a suppression list are still imported but flagged, so the reported count matches what was submitted. Account-scoped credentials only. Beta: the contact import surface may change before it is declared stable.
     * Import contacts in bulk (beta)
     * @param importContactsRequest 
     * @param idempotencyKey Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first response — including the original batch_id and per-row results — instead of importing the rows a second time. Strongly recommended: an import that times out after the rows landed is otherwise indistinguishable from one that failed.
     */
    public async importContacts(importContactsRequest: ImportContactsRequest, idempotencyKey?: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'importContactsRequest' is not null or undefined
        if (importContactsRequest === null || importContactsRequest === undefined) {
            throw new RequiredError("ContactsApi", "importContacts", "importContactsRequest");
        }



        // Path Params
        const localVarPath = '/v1/contacts/import';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.POST);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Header Params
        requestContext.setHeaderParam("Idempotency-Key", ObjectSerializer.serialize(idempotencyKey, "string", ""));


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(importContactsRequest, "ImportContactsRequest", ""),
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
     * Lists the people this account corresponds with, newest first. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * List contacts (beta)
     * @param source Filter by provenance. Known values: import, manual, inbound.
     * @param importBatchId Filter to the contacts created by one import.
     * @param createdAfter Only contacts created strictly after this instant (RFC 3339).
     * @param createdBefore Only contacts created strictly before this instant (RFC 3339).
     * @param cursor Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param limit Maximum number of items to return (1-100).
     */
    public async listContacts(source?: string, importBatchId?: string, createdAfter?: Date, createdBefore?: Date, cursor?: string, limit?: number, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;







        // Path Params
        const localVarPath = '/v1/contacts';

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.GET);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (source !== undefined) {
            requestContext.setQueryParam("source", ObjectSerializer.serialize(source, "string", ""));
        }

        // Query Params
        if (importBatchId !== undefined) {
            requestContext.setQueryParam("import_batch_id", ObjectSerializer.serialize(importBatchId, "string", ""));
        }

        // Query Params
        if (createdAfter !== undefined) {
            requestContext.setQueryParam("created_after", ObjectSerializer.serialize(createdAfter, "Date", "date-time"));
        }

        // Query Params
        if (createdBefore !== undefined) {
            requestContext.setQueryParam("created_before", ObjectSerializer.serialize(createdBefore, "Date", "date-time"));
        }

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
     * Lists the contacts this agent is working, with the reply and delivery facts e2a derives from real message activity. Combine replied=false, next_action_before and last_outbound_before to get everyone due for a follow-up in one request. Agent-scoped credentials may read their own agent. Beta: the outreach surface may change before it is declared stable.
     * List an agent\'s outreach (beta)
     * @param email 
     * @param stage Only engagements at this exact stage.
     * @param replied Filter on whether the contact has answered since outreach began. Omit for both.
     * @param suppressed Filter on whether sends are blocked. Omit for both.
     * @param nextActionBefore Only engagements whose next_action_at has passed this instant (RFC 3339). Pass the current time to get everyone due.
     * @param lastOutboundBefore Only engagements not contacted since this instant (RFC 3339). Include it alongside next_action_before: last_outbound_at is server-maintained, so it excludes anyone just contacted even if the client\&#39;s own state write was lost — without it, a failed write can cause a duplicate send.
     * @param cursor Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param limit Maximum number of items to return (1-100).
     */
    public async listEngagements(email: string, stage?: string, replied?: 'true' | 'false', suppressed?: 'true' | 'false', nextActionBefore?: Date, lastOutboundBefore?: Date, cursor?: string, limit?: number, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'email' is not null or undefined
        if (email === null || email === undefined) {
            throw new RequiredError("ContactsApi", "listEngagements", "email");
        }









        // Path Params
        const localVarPath = '/v1/agents/{email}/contacts'
            .replace('{' + 'email' + '}', encodeURIComponent(String(email)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.GET);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Query Params
        if (stage !== undefined) {
            requestContext.setQueryParam("stage", ObjectSerializer.serialize(stage, "string", ""));
        }

        // Query Params
        if (replied !== undefined) {
            requestContext.setQueryParam("replied", ObjectSerializer.serialize(replied, "'true' | 'false'", ""));
        }

        // Query Params
        if (suppressed !== undefined) {
            requestContext.setQueryParam("suppressed", ObjectSerializer.serialize(suppressed, "'true' | 'false'", ""));
        }

        // Query Params
        if (nextActionBefore !== undefined) {
            requestContext.setQueryParam("next_action_before", ObjectSerializer.serialize(nextActionBefore, "Date", "date-time"));
        }

        // Query Params
        if (lastOutboundBefore !== undefined) {
            requestContext.setQueryParam("last_outbound_before", ObjectSerializer.serialize(lastOutboundBefore, "Date", "date-time"));
        }

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
     * Partially updates a contact. Omitted fields are left unchanged. Address and provenance are immutable. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Update a contact (beta)
     * @param address 
     * @param updateContactRequest 
     * @param ifMatch Optional ETag from a prior read. When present it must still match at the instant of the write or the update is rejected with 412.
     */
    public async updateContact(address: string, updateContactRequest: UpdateContactRequest, ifMatch?: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "updateContact", "address");
        }


        // verify required parameter 'updateContactRequest' is not null or undefined
        if (updateContactRequest === null || updateContactRequest === undefined) {
            throw new RequiredError("ContactsApi", "updateContact", "updateContactRequest");
        }



        // Path Params
        const localVarPath = '/v1/contacts/{address}'
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.PATCH);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Header Params
        requestContext.setHeaderParam("If-Match", ObjectSerializer.serialize(ifMatch, "string", ""));


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(updateContactRequest, "UpdateContactRequest", ""),
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
     * Enrols a contact in this agent\'s outreach, or updates the agent-owned fields of an existing enrolment. Omitted fields are left unchanged, so advancing the stage after a send does not disturb the schedule. Creates the contact if it does not exist. Pass If-Match from a prior read to prevent a stale automation loop from overwriting newer state; a conditional request never creates. Derived fields are server-owned and rejected. Returns 201 on first enrolment and 200 on a subsequent update. Agent-scoped credentials may write their own agent. Beta: the outreach surface may change before it is declared stable.
     * Enrol or update outreach state (beta)
     * @param email 
     * @param address 
     * @param upsertEngagementRequest 
     * @param ifMatch Optional ETag from a prior read. When present the engagement must already exist and still match at the instant of the write, or the update is rejected with 412.
     */
    public async upsertEngagement(email: string, address: string, upsertEngagementRequest: UpsertEngagementRequest, ifMatch?: string, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;

        // verify required parameter 'email' is not null or undefined
        if (email === null || email === undefined) {
            throw new RequiredError("ContactsApi", "upsertEngagement", "email");
        }


        // verify required parameter 'address' is not null or undefined
        if (address === null || address === undefined) {
            throw new RequiredError("ContactsApi", "upsertEngagement", "address");
        }


        // verify required parameter 'upsertEngagementRequest' is not null or undefined
        if (upsertEngagementRequest === null || upsertEngagementRequest === undefined) {
            throw new RequiredError("ContactsApi", "upsertEngagement", "upsertEngagementRequest");
        }



        // Path Params
        const localVarPath = '/v1/agents/{email}/contacts/{address}'
            .replace('{' + 'email' + '}', encodeURIComponent(String(email)))
            .replace('{' + 'address' + '}', encodeURIComponent(String(address)));

        // Make Request Context
        const requestContext = _config.baseServer.makeRequestContext(localVarPath, HttpMethod.PUT);
        requestContext.setHeaderParam("Accept", "application/json, */*;q=0.8")

        // Header Params
        requestContext.setHeaderParam("If-Match", ObjectSerializer.serialize(ifMatch, "string", ""));


        // Body Params
        const contentType = ObjectSerializer.getPreferredMediaType([
            "application/json"
        ]);
        requestContext.setHeaderParam("Content-Type", contentType);
        const serializedBody = ObjectSerializer.stringify(
            ObjectSerializer.serialize(upsertEngagementRequest, "UpsertEngagementRequest", ""),
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

export class ContactsApiResponseProcessor {

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to createContact
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async createContactWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("201", response.httpStatusCode)) {
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
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
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to deleteContact
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async deleteContactWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DeleteContactResult >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: DeleteContactResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteContactResult", ""
            ) as DeleteContactResult;
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
            const body: DeleteContactResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteContactResult", ""
            ) as DeleteContactResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to deleteEngagement
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async deleteEngagementWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DeleteEngagementResult >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: DeleteEngagementResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteEngagementResult", ""
            ) as DeleteEngagementResult;
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
            const body: DeleteEngagementResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteEngagementResult", ""
            ) as DeleteEngagementResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to deleteImportBatch
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async deleteImportBatchWithHttpInfo(response: ResponseContext): Promise<HttpInfo<DeleteImportBatchResult >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: DeleteImportBatchResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteImportBatchResult", ""
            ) as DeleteImportBatchResult;
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
            const body: DeleteImportBatchResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "DeleteImportBatchResult", ""
            ) as DeleteImportBatchResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to getContact
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async getContactWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
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
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to getEngagement
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async getEngagementWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactEngagementView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: ContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactEngagementView", ""
            ) as ContactEngagementView;
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
            const body: ContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactEngagementView", ""
            ) as ContactEngagementView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to importContacts
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async importContactsWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactImportResult >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: ContactImportResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactImportResult", ""
            ) as ContactImportResult;
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
            const body: ContactImportResult = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactImportResult", ""
            ) as ContactImportResult;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to listContacts
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async listContactsWithHttpInfo(response: ResponseContext): Promise<HttpInfo<PageContactView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: PageContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageContactView", ""
            ) as PageContactView;
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
            const body: PageContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageContactView", ""
            ) as PageContactView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to listEngagements
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async listEngagementsWithHttpInfo(response: ResponseContext): Promise<HttpInfo<PageContactEngagementView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: PageContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageContactEngagementView", ""
            ) as PageContactEngagementView;
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
            const body: PageContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageContactEngagementView", ""
            ) as PageContactEngagementView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to updateContact
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async updateContactWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
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
            const body: ContactView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactView", ""
            ) as ContactView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to upsertEngagement
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async upsertEngagementWithHttpInfo(response: ResponseContext): Promise<HttpInfo<ContactEngagementView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: ContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactEngagementView", ""
            ) as ContactEngagementView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }
        if (isCodeInRange("201", response.httpStatusCode)) {
            const body: ContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactEngagementView", ""
            ) as ContactEngagementView;
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
            const body: ContactEngagementView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "ContactEngagementView", ""
            ) as ContactEngagementView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

}
