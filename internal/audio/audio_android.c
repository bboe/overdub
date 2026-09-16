
#include "audio.h"

#include <SLES/OpenSLES.h>
#include <SLES/OpenSLES_Android.h>

#include <string.h>

#define CHUNK 960
#define NUM_BUFFERS 8

static unsigned char pool[NUM_BUFFERS][CHUNK];
static int slot;
static int frame_bytes;

static SLObjectItf engine_obj, mix_obj, player_obj;
static SLPlayItf player_play;
static SLAndroidSimpleBufferQueueItf player_queue;

#define TRY(expr)                                   \
	do {                                            \
		if ((expr) != SL_RESULT_SUCCESS) return -1; \
	} while (0)

#define TRY_INIT(expr)                   \
	do {                                 \
		if ((expr) != SL_RESULT_SUCCESS) { \
			audio_close();               \
			return -1;                   \
		}                                \
	} while (0)

static SLuint32 milli_hz(int rate) {
	switch (rate) {
	case 8000: return SL_SAMPLINGRATE_8;
	case 16000: return SL_SAMPLINGRATE_16;
	case 22050: return SL_SAMPLINGRATE_22_05;
	case 24000: return SL_SAMPLINGRATE_24;
	case 32000: return SL_SAMPLINGRATE_32;
	case 44100: return SL_SAMPLINGRATE_44_1;
	case 48000: return SL_SAMPLINGRATE_48;
	default: return 0;
	}
}

int audio_open(int rate, int channels) {
	SLuint32 sl_rate = milli_hz(rate);
	if (sl_rate == 0 || channels < 1 || channels > 2) return -1;

	frame_bytes = channels * 2;
	slot = 0;

	TRY_INIT(slCreateEngine(&engine_obj, 0, NULL, 0, NULL, NULL));
	TRY_INIT((*engine_obj)->Realize(engine_obj, SL_BOOLEAN_FALSE));
	SLEngineItf engine;
	TRY_INIT((*engine_obj)->GetInterface(engine_obj, SL_IID_ENGINE, &engine));
	TRY_INIT((*engine)->CreateOutputMix(engine, &mix_obj, 0, NULL, NULL));
	TRY_INIT((*mix_obj)->Realize(mix_obj, SL_BOOLEAN_FALSE));

	SLDataLocator_AndroidSimpleBufferQueue loc = {
	    SL_DATALOCATOR_ANDROIDSIMPLEBUFFERQUEUE, NUM_BUFFERS};
	SLDataFormat_PCM fmt = {
	    SL_DATAFORMAT_PCM, (SLuint32)channels, sl_rate,
	    SL_PCMSAMPLEFORMAT_FIXED_16, SL_PCMSAMPLEFORMAT_FIXED_16,
	    channels == 1 ? SL_SPEAKER_FRONT_CENTER
	                  : (SL_SPEAKER_FRONT_LEFT | SL_SPEAKER_FRONT_RIGHT),
	    SL_BYTEORDER_LITTLEENDIAN};
	SLDataSource src = {&loc, &fmt};
	SLDataLocator_OutputMix omix = {SL_DATALOCATOR_OUTPUTMIX, mix_obj};
	SLDataSink sink = {&omix, NULL};

	const SLInterfaceID ids[] = {SL_IID_BUFFERQUEUE, SL_IID_ANDROIDCONFIGURATION};
	const SLboolean req[] = {SL_BOOLEAN_TRUE, SL_BOOLEAN_FALSE};
	TRY_INIT((*engine)->CreateAudioPlayer(engine, &player_obj, &src, &sink, 2, ids, req));

	SLAndroidConfigurationItf config;
	if ((*player_obj)->GetInterface(player_obj, SL_IID_ANDROIDCONFIGURATION, &config)
	    == SL_RESULT_SUCCESS) {
		SLint32 stream = SL_ANDROID_STREAM_MEDIA;
		(*config)->SetConfiguration(config, SL_ANDROID_KEY_STREAM_TYPE, &stream,
		                            sizeof(stream));
	}

	TRY_INIT((*player_obj)->Realize(player_obj, SL_BOOLEAN_FALSE));
	TRY_INIT((*player_obj)->GetInterface(player_obj, SL_IID_PLAY, &player_play));
	TRY_INIT((*player_obj)->GetInterface(player_obj, SL_IID_BUFFERQUEUE, &player_queue));
	return 0;
}

int audio_write(const unsigned char *pcm, size_t len) {
	if (player_queue == NULL) return -1;

	SLAndroidSimpleBufferQueueState state;
	TRY((*player_queue)->GetState(player_queue, &state));
	if (state.count >= NUM_BUFFERS) return 0;

	size_t n = len > CHUNK ? CHUNK : len;
	n -= n % (size_t)frame_bytes;
	if (n == 0) return -1;

	memcpy(pool[slot], pcm, n);
	TRY((*player_queue)->Enqueue(player_queue, pool[slot], n));
	slot = (slot + 1) % NUM_BUFFERS;
	return (int)n;
}

int audio_start(void) {
	if (player_play == NULL) return -1;
	TRY((*player_play)->SetPlayState(player_play, SL_PLAYSTATE_PLAYING));
	return 0;
}

void audio_close(void) {
	if (player_play != NULL) {
		(*player_play)->SetPlayState(player_play, SL_PLAYSTATE_STOPPED);
		player_play = NULL;
	}
	player_queue = NULL;
	if (player_obj != NULL) {
		(*player_obj)->Destroy(player_obj);
		player_obj = NULL;
	}
	if (mix_obj != NULL) {
		(*mix_obj)->Destroy(mix_obj);
		mix_obj = NULL;
	}
	if (engine_obj != NULL) {
		(*engine_obj)->Destroy(engine_obj);
		engine_obj = NULL;
	}
	slot = 0;
}
