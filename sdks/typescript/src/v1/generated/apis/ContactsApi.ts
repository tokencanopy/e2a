// TODO: better import syntax?
import {BaseAPIRequestFactory, RequiredError, COLLECTION_FORMATS} from './baseapi.js';
import {Configuration} from '../configuration.js';
import {RequestContext, HttpMethod, ResponseContext, HttpFile, HttpInfo} from '../http/http.js';
import {ObjectSerializer} from '../models/ObjectSerializer.js';
import {ApiException} from './exception.js';
import {canConsumeForm, isCodeInRange} from '../util.js';
import {SecurityAuthentication} from '../auth/auth.js';


import { ContactImportResult } from '../models/ContactImportResult.js';
import { ContactView } from '../models/ContactView.js';
import { CreateContactRequest } from '../models/CreateContactRequest.js';
import { DeleteContactResult } from '../models/DeleteContactResult.js';
import { DeleteImportBatchResult } from '../models/DeleteImportBatchResult.js';
import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { ImportContactsRequest } from '../models/ImportContactsRequest.js';
import { PageContactView } from '../models/PageContactView.js';
import { UpdateContactRequest } from '../models/UpdateContactRequest.js';

/**
 * no description
 */
export class ContactsApiRequestFactory extends BaseAPIRequestFactory {

    /**
     * Creates one contact. The address is canonicalized before storage, so a display-name form and the bare address are the same contact — a second create returns 409. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Create a contact (beta)
     * @param createContactRequest 
     */
    public async createContact(createContactRequest: CreateContactRequest, _options?: Configuration): Promise<RequestContext> {
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
     * Removes the contacts an import created. Requires ?confirm=DELETE. Only contacts still attributed to this batch are removed; any whose provenance has moved on are retained and counted separately. Suppressions are never affected. Account-scoped credentials only. Beta: the contact import surface may change before it is declared stable.
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
     * Creates or updates up to 1000 contacts in one request and returns a per-row outcome, so one malformed row never rejects the rest of the upload. Import is inert: it records identity and sends nothing. Addresses already on a suppression list are still imported but flagged, so the reported count matches what was submitted. Account-scoped credentials only. Beta: the contact import surface may change before it is declared stable.
     * Import contacts in bulk (beta)
     * @param importContactsRequest 
     */
    public async importContacts(importContactsRequest: ImportContactsRequest, _options?: Configuration): Promise<RequestContext> {
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
     * Partially updates a contact. Omitted fields are left unchanged. Address and provenance are immutable. Account-scoped credentials only. Beta: the contacts surface may change before it is declared stable.
     * Update a contact (beta)
     * @param address 
     * @param updateContactRequest 
     * @param ifMatch Optional ETag from a prior read. When present it must match the contact\&#39;s current ETag or the write is rejected with 412. Beta limitation: the comparison and the write are not yet a single atomic operation, so two writers racing with the same valid ETag can both be accepted; the check reliably rejects a stale read but is not a hard serialization guarantee. This will be tightened to a conditional write before contacts leave beta.
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

}
