"use strict";
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
Object.defineProperty(exports, "__esModule", { value: true });
const PropertySymbol = __importStar(require("../PropertySymbol.cjs"));
const Document_js_1 = __importDefault(require("../nodes/document/Document.cjs"));
const HTMLDocument_js_1 = __importDefault(require("../nodes/html-document/HTMLDocument.cjs"));
const XMLDocument_js_1 = __importDefault(require("../nodes/xml-document/XMLDocument.cjs"));
const DocumentFragment_js_1 = __importDefault(require("../nodes/document-fragment/DocumentFragment.cjs"));
const Text_js_1 = __importDefault(require("../nodes/text/Text.cjs"));
const Comment_js_1 = __importDefault(require("../nodes/comment/Comment.cjs"));
const Image_js_1 = __importDefault(require("../nodes/html-image-element/Image.cjs"));
const Audio_js_1 = __importDefault(require("../nodes/html-audio-element/Audio.cjs"));
const MutationObserver_js_1 = __importDefault(require("../mutation-observer/MutationObserver.cjs"));
const MessagePort_js_1 = __importDefault(require("../event/MessagePort.cjs"));
const CSSStyleSheet_js_1 = __importDefault(require("../css/CSSStyleSheet.cjs"));
const DOMException_js_1 = __importDefault(require("../exception/DOMException.cjs"));
const Request_js_1 = __importDefault(require("../fetch/Request.cjs"));
const Response_js_1 = __importDefault(require("../fetch/Response.cjs"));
const EventTarget_js_1 = __importDefault(require("../event/EventTarget.cjs"));
const XMLHttpRequestUpload_js_1 = __importDefault(require("../xml-http-request/XMLHttpRequestUpload.cjs"));
const XMLHttpRequestEventTarget_js_1 = __importDefault(require("../xml-http-request/XMLHttpRequestEventTarget.cjs"));
const AbortController_js_1 = __importDefault(require("../fetch/AbortController.cjs"));
const AbortSignal_js_1 = __importDefault(require("../fetch/AbortSignal.cjs"));
const FormData_js_1 = __importDefault(require("../form-data/FormData.cjs"));
const PermissionStatus_js_1 = __importDefault(require("../permissions/PermissionStatus.cjs"));
const XMLHttpRequest_js_1 = __importDefault(require("../xml-http-request/XMLHttpRequest.cjs"));
const DOMParser_js_1 = __importDefault(require("../dom-parser/DOMParser.cjs"));
const Range_js_1 = __importDefault(require("../range/Range.cjs"));
const VTTCue_js_1 = __importDefault(require("../nodes/html-media-element/VTTCue.cjs"));
const TextTrack_js_1 = __importDefault(require("../nodes/html-media-element/TextTrack.cjs"));
const TextTrackList_js_1 = __importDefault(require("../nodes/html-media-element/TextTrackList.cjs"));
const TextTrackCue_js_1 = __importDefault(require("../nodes/html-media-element/TextTrackCue.cjs"));
const RemotePlayback_js_1 = __importDefault(require("../nodes/html-media-element/RemotePlayback.cjs"));
const FileReader_js_1 = __importDefault(require("../file/FileReader.cjs"));
const MediaStream_js_1 = __importDefault(require("../nodes/html-media-element/MediaStream.cjs"));
const MediaStreamTrack_js_1 = __importDefault(require("../nodes/html-media-element/MediaStreamTrack.cjs"));
const CanvasCaptureMediaStreamTrack_js_1 = __importDefault(require("../nodes/html-canvas-element/CanvasCaptureMediaStreamTrack.cjs"));
/**
 * Extends classes with a "window" property, so that they internally can access it's Window context.
 *
 * By using WindowBrowserContext, the classes can get access to their Browser context, for accessing settings or navigating the browser.
 */
class WindowContextClassExtender {
    /**
     * Extends classes with a "window" property.
     *
     * @param window Window.
     */
    static extendClasses(window) {
        /* eslint-disable jsdoc/require-jsdoc */
        // Document
        class Document extends Document_js_1.default {
        }
        Document.prototype[PropertySymbol.window] = window;
        window.Document = Document;
        // HTMLDocument
        class HTMLDocument extends HTMLDocument_js_1.default {
        }
        HTMLDocument.prototype[PropertySymbol.window] = window;
        window.HTMLDocument = HTMLDocument;
        // XMLDocument
        class XMLDocument extends XMLDocument_js_1.default {
        }
        XMLDocument.prototype[PropertySymbol.window] = window;
        window.XMLDocument = XMLDocument;
        // DocumentFragment
        class DocumentFragment extends DocumentFragment_js_1.default {
        }
        DocumentFragment.prototype[PropertySymbol.window] = window;
        window.DocumentFragment = DocumentFragment;
        // Text
        class Text extends Text_js_1.default {
        }
        Text.prototype[PropertySymbol.window] = window;
        window.Text = Text;
        // Comment
        class Comment extends Comment_js_1.default {
        }
        Comment.prototype[PropertySymbol.window] = window;
        window.Comment = Comment;
        // Image
        class Image extends Image_js_1.default {
        }
        Image.prototype[PropertySymbol.window] = window;
        window.Image = Image;
        // Audio
        class Audio extends Audio_js_1.default {
        }
        Audio.prototype[PropertySymbol.window] = window;
        window.Audio = Audio;
        // MutationObserver
        class MutationObserver extends MutationObserver_js_1.default {
        }
        MutationObserver.prototype[PropertySymbol.window] = window;
        window.MutationObserver = MutationObserver;
        // MessagePort
        class MessagePort extends MessagePort_js_1.default {
        }
        MessagePort.prototype[PropertySymbol.window] = window;
        window.MessagePort = MessagePort;
        // CSSStyleSheet
        class CSSStyleSheet extends CSSStyleSheet_js_1.default {
        }
        CSSStyleSheet.prototype[PropertySymbol.window] = window;
        window.CSSStyleSheet = CSSStyleSheet;
        // DOMException
        class DOMException extends DOMException_js_1.default {
        }
        window.DOMException = DOMException;
        // Request
        class Request extends Request_js_1.default {
        }
        Request.prototype[PropertySymbol.window] = window;
        window.Request = Request;
        // Response
        class Response extends Response_js_1.default {
        }
        Response.prototype[PropertySymbol.window] = window;
        Response[PropertySymbol.window] = window;
        window.Response = Response;
        // XMLHttpRequestEventTarget
        class EventTarget extends EventTarget_js_1.default {
        }
        EventTarget.prototype[PropertySymbol.window] = window;
        window.EventTarget = EventTarget;
        // XMLHttpRequestUpload
        class XMLHttpRequestUpload extends XMLHttpRequestUpload_js_1.default {
        }
        XMLHttpRequestUpload.prototype[PropertySymbol.window] = window;
        window.XMLHttpRequestUpload = XMLHttpRequestUpload;
        // XMLHttpRequestEventTarget
        class XMLHttpRequestEventTarget extends XMLHttpRequestEventTarget_js_1.default {
        }
        XMLHttpRequestEventTarget.prototype[PropertySymbol.window] = window;
        window.XMLHttpRequestEventTarget =
            XMLHttpRequestEventTarget;
        // AbortController
        class AbortController extends AbortController_js_1.default {
        }
        AbortController.prototype[PropertySymbol.window] = window;
        window.AbortController = AbortController;
        // AbortSignal
        class AbortSignal extends AbortSignal_js_1.default {
        }
        AbortSignal.prototype[PropertySymbol.window] = window;
        AbortSignal[PropertySymbol.window] = window;
        window.AbortSignal = AbortSignal;
        // FormData
        class FormData extends FormData_js_1.default {
        }
        FormData.prototype[PropertySymbol.window] = window;
        window.FormData = FormData;
        // PermissionStatus
        class PermissionStatus extends PermissionStatus_js_1.default {
        }
        PermissionStatus.prototype[PropertySymbol.window] = window;
        window.PermissionStatus = PermissionStatus;
        // XMLHttpRequest
        class XMLHttpRequest extends XMLHttpRequest_js_1.default {
        }
        XMLHttpRequest.prototype[PropertySymbol.window] = window;
        window.XMLHttpRequest = XMLHttpRequest;
        // DOMParser
        class DOMParser extends DOMParser_js_1.default {
        }
        DOMParser.prototype[PropertySymbol.window] = window;
        window.DOMParser = DOMParser;
        // Range
        class Range extends Range_js_1.default {
        }
        Range.prototype[PropertySymbol.window] = window;
        window.Range = Range;
        // VTTCue
        class VTTCue extends VTTCue_js_1.default {
        }
        VTTCue.prototype[PropertySymbol.window] = window;
        window.VTTCue = VTTCue;
        // TextTrack
        class TextTrack extends TextTrack_js_1.default {
        }
        TextTrack.prototype[PropertySymbol.window] = window;
        window.TextTrack = TextTrack;
        // TextTrackList
        class TextTrackList extends TextTrackList_js_1.default {
        }
        TextTrackList.prototype[PropertySymbol.window] = window;
        window.TextTrackList = TextTrackList;
        // TextTrackCue
        class TextTrackCue extends TextTrackCue_js_1.default {
        }
        TextTrackCue.prototype[PropertySymbol.window] = window;
        window.TextTrackCue = TextTrackCue;
        // RemotePlayback
        class RemotePlayback extends RemotePlayback_js_1.default {
        }
        RemotePlayback.prototype[PropertySymbol.window] = window;
        window.RemotePlayback = RemotePlayback;
        // FileReader
        class FileReader extends FileReader_js_1.default {
        }
        FileReader.prototype[PropertySymbol.window] = window;
        window.FileReader = FileReader;
        // MediaStream
        class MediaStream extends MediaStream_js_1.default {
        }
        MediaStream.prototype[PropertySymbol.window] = window;
        window.MediaStream = MediaStream;
        // MediaStreamTrack
        class MediaStreamTrack extends MediaStreamTrack_js_1.default {
        }
        MediaStreamTrack.prototype[PropertySymbol.window] = window;
        window.MediaStreamTrack = MediaStreamTrack;
        // MediaStreamTrack
        class CanvasCaptureMediaStreamTrack extends CanvasCaptureMediaStreamTrack_js_1.default {
        }
        CanvasCaptureMediaStreamTrack.prototype[PropertySymbol.window] = window;
        window.CanvasCaptureMediaStreamTrack =
            CanvasCaptureMediaStreamTrack;
        /* eslint-enable jsdoc/require-jsdoc */
    }
}
exports.default = WindowContextClassExtender;
//# sourceMappingURL=WindowContextClassExtender.cjs.map