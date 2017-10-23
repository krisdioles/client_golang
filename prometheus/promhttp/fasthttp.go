package promhttp

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/valyala/fasthttp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// FastHttpHandler is a tweak fom Handler() for supporting fasthttp the prometheus.DefaultGatherer. The
// Handler uses the default HandlerOpts, i.e. report the first error as an HTTP
// error, no error logging, and compression if requested by the client.
//
// If you want to create a Handler for the DefaultGatherer with different
// HandlerOpts, create it with HandlerFor with prometheus.DefaultGatherer and
// your desired HandlerOpts.
func FastHttpHandler(ctx *fasthttp.RequestCtx) {
	FastHttpHandlerFor(ctx, prometheus.DefaultGatherer, HandlerOpts{})
	return
}

// HandlerFor returns an http.Handler for the provided Gatherer. The behavior
// of the Handler is defined by the provided HandlerOpts.
func FastHttpHandlerFor(ctx *fasthttp.RequestCtx, reg prometheus.Gatherer, opts HandlerOpts) {

	mfs, err := reg.Gather()
	if err != nil {
		if opts.ErrorLog != nil {
			opts.ErrorLog.Println("error gathering metrics:", err)
		}
		switch opts.ErrorHandling {
		case PanicOnError:
			panic(err)
		case ContinueOnError:
			if len(mfs) == 0 {
				ctx.Error("No metrics gathered, last error:\n\n"+err.Error(), http.StatusInternalServerError)
				return
			}
		case HTTPErrorOnError:
			ctx.Error("An error has occurred during metrics gathering:\n\n"+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// converting fasthttp header to net/http header
	header := make(http.Header)
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		key := string(k)
		value := string(v)

		header.Set(key, value)
	})

	contentType := expfmt.Negotiate(header)
	buf := getBuf()
	defer giveBuf(buf)
	writer, encoding := decorateFastHttpWriter(ctx, buf, opts.DisableCompression)
	enc := expfmt.NewEncoder(writer, contentType)
	var lastErr error
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			lastErr = err
			if opts.ErrorLog != nil {
				opts.ErrorLog.Println("error encoding metric family:", err)
			}
			switch opts.ErrorHandling {
			case PanicOnError:
				panic(err)
			case ContinueOnError:
				// Handled later.
			case HTTPErrorOnError:
				ctx.Error("An error has occurred during metrics encoding:\n\n"+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	if closer, ok := writer.(io.Closer); ok {
		closer.Close()
	}
	if lastErr != nil && buf.Len() == 0 {
		ctx.Error("No metrics encoded, last error:\n\n"+lastErr.Error(), http.StatusInternalServerError)
		return
	}
	ctx.Response.Header.Set(contentTypeHeader, string(contentType))
	ctx.Response.Header.Set(contentLengthHeader, fmt.Sprint(buf.Len()))
	if encoding != "" {
		ctx.Response.Header.Set(contentEncodingHeader, encoding)
	}
	ctx.Write(buf.Bytes())
	// TODO(beorn7): Consider streaming serving of metrics.
}

// decorateFastHttpWriter wraps a fast http writer to handle gzip compression if requested.  It
// returns the decorated writer and the appropriate "Content-Encoding" header
// (which is empty if no compression is enabled).
func decorateFastHttpWriter(ctx *fasthttp.RequestCtx, writer io.Writer, compressionDisabled bool) (io.Writer, string) {
	if compressionDisabled {
		return writer, ""
	}
	header := string(ctx.Request.Header.Peek(acceptEncodingHeader))
	parts := strings.Split(header, ",")
	for _, part := range parts {
		part := strings.TrimSpace(part)
		if part == "gzip" || strings.HasPrefix(part, "gzip;") {
			return gzip.NewWriter(writer), "gzip"
		}
	}
	return writer, ""
}
