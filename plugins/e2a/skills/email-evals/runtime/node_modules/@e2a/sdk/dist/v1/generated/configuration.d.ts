import { HttpLibrary } from "./http/http.js";
import { Middleware, PromiseMiddleware } from "./middleware.js";
import { BaseServerConfiguration } from "./servers.js";
import { AuthMethods, AuthMethodsConfiguration } from "./auth/auth.js";
export interface Configuration<M = Middleware> {
    readonly baseServer: BaseServerConfiguration;
    readonly httpApi: HttpLibrary;
    readonly middleware: M[];
    readonly authMethods: AuthMethods;
}
export interface MiddlewareMergeOptions {
    middlewareMergeStrategy?: "replace" | "append" | "prepend";
}
export type ConfigurationOptions<M = Middleware> = Partial<Configuration<M>> & MiddlewareMergeOptions;
export type StandardConfigurationOptions = ConfigurationOptions<Middleware>;
export type PromiseConfigurationOptions = ConfigurationOptions<PromiseMiddleware>;
/**
 * Interface with which a configuration object can be configured.
 */
export interface ConfigurationParameters {
    /**
     * Default server to use - a list of available servers (according to the
     * OpenAPI yaml definition) is included in the `servers` const in `./servers`. You can also
     * create your own server with the `ServerConfiguration` class from the same
     * file.
     */
    baseServer?: BaseServerConfiguration;
    /**
     * HTTP library to use e.g. IsomorphicFetch. This can usually be skipped as
     * all generators come with a default library.
     * If available, additional libraries can be imported from `./http/*`
     */
    httpApi?: HttpLibrary;
    /**
     * The middlewares which will be applied to requests and responses. You can
     * add any number of middleware components to modify requests before they
     * are sent or before they are deserialized by implementing the `Middleware`
     * interface defined in `./middleware`
     */
    middleware?: Middleware[];
    /**
     * Configures middleware functions that return promises instead of
     * Observables (which are used by `middleware`). Otherwise allows for the
     * same functionality as `middleware`, i.e., modifying requests before they
     * are sent and before they are deserialized.
     */
    promiseMiddleware?: PromiseMiddleware[];
    /**
     * Configuration for the available authentication methods (e.g., api keys)
     * according to the OpenAPI yaml definition. For the definition, please refer to
     * `./auth/auth`
     */
    authMethods?: AuthMethodsConfiguration;
}
/**
 * Provide your `ConfigurationParameters` to this function to get a `Configuration`
 * object that can be used to configure your APIs (in the constructor or
 * for each request individually).
 *
 * If a property is not included in conf, a default is used:
 *    - baseServer: server1
 *    - httpApi: IsomorphicFetchHttpLibrary
 *    - middleware: []
 *    - promiseMiddleware: []
 *    - authMethods: {}
 *
 * @param conf partial configuration
 */
export declare function createConfiguration(conf?: ConfigurationParameters): Configuration;
/**
 * Merge configuration options into a configuration.
 */
export declare function mergeConfiguration(conf: Configuration, options?: ConfigurationOptions): Configuration;
/**
 * Convert Promise-based configuration options to Observable-based configuration options.
 */
export declare function wrapOptions(options?: PromiseConfigurationOptions): StandardConfigurationOptions | undefined;
