import URL from '../../url/URL.cjs';
import ICookie from '../ICookie.cjs';
import Location from '../../location/Location.cjs';
/**
 * Cookie string.
 */
export default class CookieStringUtility {
    /**
     * Returns cookie.
     *
     * @param originURL Origin URL.
     * @param cookieString Cookie string.
     * @returns Cookie.
     */
    static stringToCookie(originURL: URL | Location, cookieString: string): ICookie | null;
    /**
     * Returns cookie string with key and value.
     *
     * @param cookies Cookies.
     * @returns Cookie string.
     */
    static cookiesToString(cookies: ICookie[]): string;
}
//# sourceMappingURL=CookieStringUtility.d.ts.map