import { from } from './rxjsStub.js';
export class PromiseMiddlewareWrapper {
    middleware;
    constructor(middleware) {
        this.middleware = middleware;
    }
    pre(context) {
        return from(this.middleware.pre(context));
    }
    post(context) {
        return from(this.middleware.post(context));
    }
}
