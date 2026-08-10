import { Configuration } from '../configuration.js';
/**
 *
 * @export
 */
export declare const COLLECTION_FORMATS: {
    csv: string;
    ssv: string;
    tsv: string;
    pipes: string;
};
/**
 *
 * @export
 * @class BaseAPI
 */
export declare class BaseAPIRequestFactory {
    protected configuration: Configuration;
    constructor(configuration: Configuration);
}
/**
 *
 * @export
 * @class RequiredError
 * @extends {Error}
 */
export declare class RequiredError extends Error {
    api: string;
    method: string;
    field: string;
    name: "RequiredError";
    constructor(api: string, method: string, field: string);
}
