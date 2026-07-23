package agent

import "net/http"

func (g *RelayGateway) registerAsyncDeliveryHandlers(mux *http.ServeMux) {
	mux.HandleFunc(asyncDeliveryRegisterPath, g.serveAsyncDeliveryRegister)
	mux.HandleFunc(asyncDeliveryStatusPath, g.serveAsyncDeliveryStatus)
	mux.HandleFunc(asyncDeliveryDeliverPath, g.serveAsyncDeliveryDeliver)
	mux.HandleFunc(asyncDeliveryDeliverV2Path, g.serveAsyncDeliveryDeliverV2)
	mux.HandleFunc(asyncStickerSearchPath, g.serveAsyncStickerSearch)
	mux.HandleFunc(asyncStickerSelectPath, g.serveAsyncStickerSelect)
	mux.HandleFunc(asyncStickerSendPath, g.serveAsyncStickerSend)
	if g.config.Videos != nil {
		mux.HandleFunc(asyncVideoSearchPath, g.serveAsyncVideoSearch)
		mux.HandleFunc(asyncVideoInspectPath, g.serveAsyncVideoInspect)
		mux.HandleFunc(asyncVideoSendPath, g.serveAsyncVideoSend)
		mux.HandleFunc(asyncVideoSendURLPath, g.serveAsyncVideoSendURL)
		mux.HandleFunc(asyncVideoStatusPath, g.serveAsyncVideoStatus)
	}
	mux.HandleFunc(asyncDeliveryRevokePath, g.serveAsyncDeliveryRevoke)
	mux.HandleFunc(asyncDeliveryReconcilePath, g.serveAsyncDeliveryReconcile)
}
