import { RequestContext, HttpMethod } from "./http/http.js";
export interface BaseServerConfiguration {
    makeRequestContext(endpoint: string, httpMethod: HttpMethod): RequestContext;
}
/**
 *
 * Represents the configuration of a server including its
 * url template and variable configuration based on the url.
 *
 */
export declare class ServerConfiguration<T extends {
    [key: string]: string;
}> implements BaseServerConfiguration {
    private url;
    private variableConfiguration;
    constructor(url: string, variableConfiguration: T);
    /**
     * Sets the value of the variables of this server. Variables are included in
     * the `url` of this ServerConfiguration in the form `{variableName}`
     *
     * @param variableConfiguration a partial variable configuration for the
     * variables contained in the url
     */
    setVariables(variableConfiguration: Partial<T>): void;
    getConfiguration(): T;
    private getUrl;
    /**
     * Creates a new request context for this server using the url with variables
     * replaced with their respective values and the endpoint of the request appended.
     *
     * @param endpoint the endpoint to be queried on the server
     * @param httpMethod httpMethod to be used
     *
     */
    makeRequestContext(endpoint: string, httpMethod: HttpMethod): RequestContext;
}
export declare const server1: ServerConfiguration<{}>;
export declare const servers: ServerConfiguration<{}>[];
