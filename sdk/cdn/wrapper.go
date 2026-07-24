package cdn

import (
	"bytes"
	"context"
	"io"

	"github.com/sbgayhub/golem/sdk"
)

// GRPCClient 实现 Ability 接口，通过 gRPC 调用远程 CDN 服务
type GRPCClient struct {
	Client CDNServiceClient
}

var _ Ability = (*GRPCClient)(nil)

// UploadImage 客户端流式上传聊天图片
func (c GRPCClient) UploadImage(receiver string, reader io.Reader) (*UploadImage_Response, error) {
	stream, err := c.Client.UploadImage(context.Background())
	if err != nil {
		return nil, err
	}

	// 发送元数据
	if err := stream.Send(&UploadImage_Chunk{Receiver: receiver}); err != nil {
		return nil, err
	}

	// 发送数据块
	buf := make([]byte, sdk.PROTO_STREAM_CHUNK_SIZE)
	for {
		if n, err := reader.Read(buf); n > 0 {
			if sendErr := stream.Send(&UploadImage_Chunk{Data: buf[:n]}); sendErr != nil {
				return nil, sendErr
			}
		} else if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
	}

	return stream.CloseAndRecv()
}

// UploadVideo 客户端流式上传聊天视频
func (c GRPCClient) UploadVideo(receiver string, thumb []byte, reader io.Reader, duration uint32) (*UploadVideo_Response, error) {
	stream, err := c.Client.UploadVideo(context.Background())
	if err != nil {
		return nil, err
	}

	// 发送元数据
	chunk := UploadVideo_Chunk{
		Receiver: receiver,
		Duration: duration,
		Thumb:    thumb,
	}
	if err := stream.Send(&chunk); err != nil {
		return nil, err
	}

	// 发送视频数据
	buf := make([]byte, sdk.PROTO_STREAM_CHUNK_SIZE)
	for {
		if n, err := reader.Read(buf); n > 0 {
			if sendErr := stream.Send(&UploadVideo_Chunk{Data: buf[:n]}); sendErr != nil {
				return nil, sendErr
			}
		} else if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
	}

	return stream.CloseAndRecv()
}

// DownloadImage 服务端流式下载高清图片
func (c GRPCClient) DownloadImage(fileID, fileAesKey string) (io.ReadCloser, error) {
	return c.DownloadImageContext(context.Background(), fileID, fileAesKey)
}

// DownloadImageContext is the cancellation-aware variant used by callers that
// expose a request-scoped media capability.  Keep DownloadImage above for
// backwards compatibility with the public Ability interface, but never force
// a capability request to inherit an unbounded background gRPC stream.
func (c GRPCClient) DownloadImageContext(ctx context.Context, fileID, fileAesKey string) (io.ReadCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stream, err := c.Client.DownloadImage(ctx, &DownloadImage_Request{
		FileId: fileID,
		AesKey: fileAesKey,
	})
	if err != nil {
		return nil, err
	}

	// 接收数据并写入 buffer
	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		buf.Write(chunk.Data)
	}

	return io.NopCloser(&buf), nil
}

// DownloadVideo 服务端流式下载聊天视频
func (c GRPCClient) DownloadVideo(fileID, fileAesKey string) (io.ReadCloser, error) {
	stream, err := c.Client.DownloadVideo(context.Background(), &DownloadVideo_Request{
		FileId: fileID,
		AesKey: fileAesKey,
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		buf.Write(chunk.Data)
	}

	return io.NopCloser(&buf), nil
}

// UploadMomentsImage 上传朋友圈图片（小文件，非流式）
func (c GRPCClient) UploadMomentsImage(imageData []byte) (*UploadMomentsImage_Response, error) {
	return c.Client.UploadMomentsImage(context.Background(), &UploadMomentsImage_Request{Data: imageData})
}

// UploadMomentsVideo 上传朋友圈视频（小文件，非流式）
func (c GRPCClient) UploadMomentsVideo(videoData, thumbData []byte) (*UploadMomentsVideo_Response, error) {
	return c.Client.UploadMomentsVideo(context.Background(), &UploadMomentsVideo_Request{
		VideoData: videoData,
		ThumbData: thumbData,
	})
}

// DownloadVideoCover 下载视频封面（小文件，非流式）
func (c GRPCClient) DownloadVideoCover(fileID, fileAesKey string) ([]byte, error) {
	resp, err := c.Client.DownloadVideoCover(context.Background(), &DownloadVideoCover_Request{
		FileId: fileID,
		AesKey: fileAesKey,
	})
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// DownloadMomentsVideo 下载朋友圈视频（小文件，非流式）
func (c GRPCClient) DownloadMomentsVideo(videoURL string, encKey uint64) ([]byte, error) {
	resp, err := c.Client.DownloadMomentsVideo(context.Background(), &DownloadMomentsVideo_Request{
		VideoUrl: videoURL,
		EncKey:   encKey,
	})
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Server 实现 CDNServiceServer 接口，将 gRPC 请求委托给 Ability 实现
type Server struct {
	UnimplementedCDNServiceServer
	Impl Ability
}

// UploadImage 客户端流式上传聊天图片
func (s Server) UploadImage(stream CDNService_UploadImageServer) error {
	// 接收元数据
	chunk, err := stream.Recv()
	if err != nil {
		return err
	}
	receiver := chunk.GetReceiver()

	// 接收数据
	var buf bytes.Buffer
	for {
		if chunk, err := stream.Recv(); err == nil {
			buf.Write(chunk.GetData())
		} else if err == io.EOF {
			break
		} else {
			return err
		}
	}

	// 调用 Ability 实现
	result, err := s.Impl.UploadImage(receiver, &buf)
	if err != nil {
		return err
	}

	return stream.SendAndClose(result)
}

// UploadVideo 客户端流式上传聊天视频
func (s Server) UploadVideo(stream CDNService_UploadVideoServer) error {
	// 接收元数据
	chunk, err := stream.Recv()
	if err != nil {
		return err
	}

	// 接收数据
	var buf bytes.Buffer
	for {
		if chunk, err := stream.Recv(); err == nil {
			buf.Write(chunk.GetData())
		} else if err == io.EOF {
			break
		} else {
			return err
		}
	}

	// 调用 Ability 实现
	result, err := s.Impl.UploadVideo(chunk.Receiver, chunk.Thumb, &buf, chunk.Duration)
	if err != nil {
		return err
	}

	return stream.SendAndClose(result)
}

// DownloadImage 服务端流式下载高清图片
func (s Server) DownloadImage(request *DownloadImage_Request, stream CDNService_DownloadImageServer) error {
	reader, err := s.Impl.DownloadImage(request.FileId, request.AesKey)
	if err != nil {
		return err
	}
	defer reader.Close()

	buf := make([]byte, sdk.PROTO_STREAM_CHUNK_SIZE)
	for {
		if n, err := reader.Read(buf); n > 0 {
			if sendErr := stream.Send(&DownloadImage_Chunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		} else if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}

	return nil
}

// DownloadVideo 服务端流式下载聊天视频
func (s Server) DownloadVideo(request *DownloadVideo_Request, stream CDNService_DownloadVideoServer) error {
	reader, err := s.Impl.DownloadVideo(request.FileId, request.AesKey)
	if err != nil {
		return err
	}
	defer reader.Close()

	buf := make([]byte, sdk.PROTO_STREAM_CHUNK_SIZE)
	for {
		if n, err := reader.Read(buf); n > 0 {
			if sendErr := stream.Send(&DownloadVideo_Chunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		} else if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
	}

	return nil
}

// UploadMomentsImage 上传朋友圈图片（小文件，非流式）
func (s Server) UploadMomentsImage(ctx context.Context, request *UploadMomentsImage_Request) (*UploadMomentsImage_Response, error) {
	return s.Impl.UploadMomentsImage(request.Data)
}

// UploadMomentsVideo 上传朋友圈视频（小文件，非流式）
func (s Server) UploadMomentsVideo(ctx context.Context, request *UploadMomentsVideo_Request) (*UploadMomentsVideo_Response, error) {
	return s.Impl.UploadMomentsVideo(request.VideoData, request.ThumbData)
}

// DownloadVideoCover 下载视频封面（小文件，非流式）
func (s Server) DownloadVideoCover(ctx context.Context, request *DownloadVideoCover_Request) (*DownloadVideoCover_Response, error) {
	data, err := s.Impl.DownloadVideoCover(request.FileId, request.AesKey)
	if err != nil {
		return nil, err
	}
	return &DownloadVideoCover_Response{Data: data}, nil
}

// DownloadSnsVideo 下载朋友圈视频（小文件，非流式）
func (s Server) DownloadSnsVideo(ctx context.Context, request *DownloadMomentsVideo_Request) (*DownloadMomentsVideo_Response, error) {
	data, err := s.Impl.DownloadMomentsVideo(request.VideoUrl, request.EncKey)
	if err != nil {
		return nil, err
	}
	return &DownloadMomentsVideo_Response{Data: data}, nil
}
