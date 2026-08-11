import { HttpLibrary, RequestContext, ResponseContext } from './http.js';
import { Observable } from '../rxjsStub.js';
export declare class IsomorphicFetchHttpLibrary implements HttpLibrary {
    send(request: RequestContext): Observable<ResponseContext>;
}
