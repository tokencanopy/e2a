// TODO: better import syntax?
import {BaseAPIRequestFactory, RequiredError, COLLECTION_FORMATS} from './baseapi.js';
import {Configuration} from '../configuration.js';
import {RequestContext, HttpMethod, ResponseContext, HttpFile, HttpInfo} from '../http/http.js';
import {ObjectSerializer} from '../models/ObjectSerializer.js';
import {ApiException} from './exception.js';
import {canConsumeForm, isCodeInRange} from '../util.js';
import {SecurityAuthentication} from '../auth/auth.js';


import { ErrorEnvelope } from '../models/ErrorEnvelope.js';
import { PageScheduledMessageView } from '../models/PageScheduledMessageView.js';

/**
 * no description
 */
export class ScheduledApiRequestFactory extends BaseAPIRequestFactory {

    /**
     * The scheduled-send queue: every outbound message accepted and awaiting its scheduled send, across the account\'s inboxes, soonest-first. Includes overdue-but-pending sends — a scheduled_at in the past means the send is still queued but its fire time has passed (e.g. deferred by the daily send cap), shown here rather than hidden until it fires. Account-scoped credentials only. Disjoint from GET /v1/reviews — held drafts are not yet accepted and appear there instead. Beta: scheduled sending is unstable — its shape may change before it is declared stable.
     * List messages awaiting a scheduled send (beta)
     * @param cursor Opaque pagination cursor from a previous response\&#39;s next_cursor. Continuation requests must not change the other filters.
     * @param limit Maximum number of items to return (1-100).
     */
    public async listScheduledMessages(cursor?: string, limit?: number, _options?: Configuration): Promise<RequestContext> {
        let _config = _options || this.configuration;



        // Path Params
        const localVarPath = '/v1/scheduled-messages';

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

}

export class ScheduledApiResponseProcessor {

    /**
     * Unwraps the actual response sent by the server from the response context and deserializes the response content
     * to the expected objects
     *
     * @params response Response returned by the server for a request to listScheduledMessages
     * @throws ApiException if the response code was not in [200, 299]
     */
     public async listScheduledMessagesWithHttpInfo(response: ResponseContext): Promise<HttpInfo<PageScheduledMessageView >> {
        const contentType = ObjectSerializer.normalizeMediaType(response.headers["content-type"]);
        if (isCodeInRange("200", response.httpStatusCode)) {
            const body: PageScheduledMessageView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageScheduledMessageView", ""
            ) as PageScheduledMessageView;
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
            const body: PageScheduledMessageView = ObjectSerializer.deserialize(
                ObjectSerializer.parse(await response.body.text(), contentType),
                "PageScheduledMessageView", ""
            ) as PageScheduledMessageView;
            return new HttpInfo(response.httpStatusCode, response.headers, response.body, body);
        }

        throw new ApiException<string | Blob | undefined>(response.httpStatusCode, "Unknown API Status Code!", await response.getBodyAsAny(), response.headers);
    }

}
