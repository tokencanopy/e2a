/**
 *
 * @export
 */
export const COLLECTION_FORMATS = {
    csv: ",",
    ssv: " ",
    tsv: "\t",
    pipes: "|",
};
/**
 *
 * @export
 * @class BaseAPI
 */
export class BaseAPIRequestFactory {
    configuration;
    constructor(configuration) {
        this.configuration = configuration;
    }
}
;
/**
 *
 * @export
 * @class RequiredError
 * @extends {Error}
 */
export class RequiredError extends Error {
    api;
    method;
    field;
    name = "RequiredError";
    constructor(api, method, field) {
        super("Required parameter " + field + " was null or undefined when calling " + api + "." + method + ".");
        this.api = api;
        this.method = method;
        this.field = field;
    }
}
