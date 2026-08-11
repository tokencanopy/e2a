/**
 * Represents an error caused by an api call i.e. it has attributes for a HTTP status code
 * and the returned body object.
 *
 * Example
 * API returns a ErrorMessageObject whenever HTTP status code is not in [200, 299]
 * => ApiException(404, someErrorMessageObject)
 *
 */
export class ApiException extends Error {
    code;
    body;
    headers;
    constructor(code, message, body, headers) {
        super("HTTP-Code: " + code + "\nMessage: " + message + "\nBody: " + JSON.stringify(body) + "\nHeaders: " +
            JSON.stringify(headers));
        this.code = code;
        this.body = body;
        this.headers = headers;
    }
}
